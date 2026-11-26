package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestProviderSettlesCreatedSandboxWhenProjectBindingChanges(t *testing.T) {
	const created, replacement = "created-box", "winning-box"
	saveError := errors.New("lost the save result")
	for _, tc := range []struct {
		name, retained string
		cancel         bool
		saveFailure    error
		wantDeletes    int32
	}{
		{name: "reconnect winner", retained: replacement, wantDeletes: 1},
		{name: "discard unused sandbox after cancellation", retained: replacement, cancel: true, wantDeletes: 1},
		{name: "keep concurrent idempotent create after cancellation", retained: created, cancel: true},
		{name: "keep committed binding after ambiguous save failure", retained: created, saveFailure: saveError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deletes, connects atomic.Int32
			mux := http.NewServeMux()
			reply := func(w http.ResponseWriter, id string) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = fmt.Fprintf(w, `{"sandboxID":%q,"envdVersion":"0.11.0","envdAccessToken":"token"}`, id)
			}
			mux.HandleFunc("POST /v2/sandboxes", func(w http.ResponseWriter, _ *http.Request) {
				reply(w, created)
			})
			mux.HandleFunc("POST /v2/sandboxes/"+tc.retained+"/connect", func(w http.ResponseWriter, _ *http.Request) {
				connects.Add(1)
				reply(w, tc.retained)
			})
			mux.HandleFunc("DELETE /sandboxes/{id}", func(w http.ResponseWriter, request *http.Request) {
				if got := request.PathValue("id"); got != created || got == tc.retained {
					t.Errorf("attempted to delete %s with retained sandbox %s", got, tc.retained)
				}
				deletes.Add(1)
				w.WriteHeader(http.StatusNoContent)
			})
			server := httptest.NewServer(mux)
			t.Cleanup(server.Close)
			store := openStore(t, ":memory:")
			seedWorkspaceOwner(t, store, "owner", "project")
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			conflict := bindingRaceStore{
				WorkspaceStore: store.E2B, winner: tc.retained, saveFailure: tc.saveFailure,
			}
			if tc.cancel {
				conflict.afterSave = cancel
			}
			const cleanupTimeout = 2 * time.Second
			cleanupDeadlines := make(chan time.Time, 1)
			client := *server.Client()
			client.Transport = cleanupProbeTransport{
				RoundTripper: client.Transport, pathSuffix: "/sandboxes/" + created, deadlines: cleanupDeadlines,
			}
			provider, err := New(Config{APIKey: "fixture", BaseURL: server.URL, ProxyURL: server.URL,
				APIVersion: "v2", WorkDirectory: "/workspace", CleanupTimeout: cleanupTimeout}, &client, conflict)
			if err != nil {
				t.Fatal(err)
			}
			spec := sandbox.Spec{RunID: "run", OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared}
			ws, err := provider.Acquire(ctx, spec)
			switch {
			case tc.cancel:
				if !errors.Is(err, context.Canceled) || ws != nil {
					t.Fatalf("canceled acquire returned %v, %v", ws, err)
				}
			case tc.saveFailure != nil:
				if !errors.Is(err, tc.saveFailure) || ws != nil {
					t.Fatalf("failed acquire returned %v, %v", ws, err)
				}
			default:
				if err != nil || ws == nil || connects.Load() != 1 {
					t.Fatalf("concurrent acquire returned %v, %v after %d reconnects", ws, err, connects.Load())
				}
				if err := ws.Close(ctx); err != nil {
					t.Fatal(err)
				}
			}
			if deletes.Load() != tc.wantDeletes || (ws == nil && connects.Load() != 0) {
				t.Fatalf("concurrent create made %d deletes and %d reconnects", deletes.Load(), connects.Load())
			}
			if tc.wantDeletes != 0 {
				select {
				case deadline := <-cleanupDeadlines:
					if remaining := time.Until(deadline); remaining <= 0 || remaining > cleanupTimeout {
						t.Errorf("cleanup time remaining = %v, want a deadline within %v", remaining, cleanupTimeout)
					}
				default:
					t.Error("missing deadline on the E2B cleanup request")
				}
			}
			id, _, err := store.E2B.E2BSandbox(t.Context(), spec.OwnerID, spec.ProjectID, server.URL)
			if err != nil || id != tc.retained {
				t.Fatalf("retained sandbox = %q, %v", id, err)
			}
		})
	}
}

type bindingRaceStore struct {
	WorkspaceStore
	winner      string
	afterSave   func()
	saveFailure error
}

func (r bindingRaceStore) SaveE2BSandbox(ctx context.Context, owner, project, endpoint, previous, _ string, data []byte) (bool, error) {
	saved, err := r.WorkspaceStore.SaveE2BSandbox(ctx, owner, project, endpoint, previous, r.winner, data)
	if err != nil || !saved {
		return saved, err
	}
	if r.afterSave != nil {
		r.afterSave()
	}
	return false, r.saveFailure
}

func TestProviderReconnectsProjectSandboxAfterRestart(t *testing.T) {
	var created, connected atomic.Int32
	var discovered atomic.Int32
	var temporaryFailure atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v2/sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TemplateID          string
			Timeout             int
			AutoPause           bool
			AllowInternetAccess json.RawMessage            `json:"allow_internet_access"`
			Network             map[string]json.RawMessage `json:"network"`
		}
		err := json.NewDecoder(r.Body).Decode(&body)
		if err != nil || body.TemplateID != "base" || body.Timeout != 300 || !body.AutoPause {
			t.Errorf("unexpected create body: %+v, %v", body, err)
		}
		if string(body.AllowInternetAccess) != "true" || len(body.Network) != 1 || string(body.Network["allowPublicTraffic"]) != "false" {
			t.Error("sandbox creation must request outbound internet and private ingress")
		}
		if r.Header.Get("X-API-Key") != "cloud-key" || r.Header.Get("Idempotency-Key") != "run-id" {
			t.Error("missing E2B control-plane headers")
		}
		created.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sandboxID":"test-box","envdVersion":"0.5.7","envdAccessToken":"sandbox-token","trafficAccessToken":"traffic-token","domain":"sandbox.example"}`)
	})
	mux.HandleFunc("POST /v2/sandboxes/test-box/connect", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Timeout int }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Timeout != 300 {
			t.Errorf("unexpected connect body: %+v, %v", body, err)
		}
		if temporaryFailure.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		connected.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sandboxID":"test-box","envdVersion":"0.5.7","envdAccessToken":"resumed-token","trafficAccessToken":"traffic-token","domain":"sandbox.example"}`)
	})
	mux.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) {
		token := "sandbox-token"
		if connected.Load() > 0 {
			token = "resumed-token"
		}
		if r.Header.Get("Authorization") != "" || r.URL.Query().Get("username") != "" {
			t.Error("new workspaces must inherit the template user")
		}
		if r.Header.Get("X-Access-Token") != token || r.Header.Get("E2b-Traffic-Access-Token") != "traffic-token" ||
			r.Header.Get("X-API-Key") != "" ||
			r.Header.Get("E2b-Sandbox-Id") != "test-box" || r.URL.Query().Get("path") != "/home/workbench/hello.txt" {
			t.Error("unexpected envd data-plane request")
		}
		_, _ = io.WriteString(w, "hello")
	})
	mux.HandleFunc("POST /process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		request := readStart(t, r)
		if request.Process.Cmd != "sh" || request.Process.Cwd != "" || r.Header.Get("Authorization") != "" {
			t.Error("directory discovery must use the template user and directory")
		}
		discovered.Add(1)
		beginProcess(t, w)
		processOutput(t, w, "stdout", "/home/workbench\n")
		finishProcess(t, w, 0)
	})
	mux.HandleFunc("DELETE /sandboxes/test-box", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("the retained sandbox was deleted")
		w.WriteHeader(http.StatusNoContent)
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("E2B_API_URL", server.URL)
	t.Setenv("E2B_DEBUG", "")
	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	dataClient := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
		if (r.URL.Host != "api.e2b.app" && r.URL.Host != "49983-test-box.sandbox.example") || r.URL.Scheme != "https" {
			t.Errorf("unexpected envd URL: %s", r.URL)
		}
		req := r.Clone(r.Context())
		req.URL.Scheme, req.URL.Host = target.Scheme, target.Host
		return http.DefaultTransport.RoundTrip(req)
	})}

	ctx := t.Context()
	database := filepath.Join(t.TempDir(), "shinona.db")
	repo := openStore(t, database)
	config := Config{APIKey: "cloud-key", Template: "base", BaseURL: "https://api.e2b.app"}
	provider, err := New(config, dataClient, repo.E2B)
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{RunID: "run-id", OwnerID: "alice", ProjectID: "web", SessionID: "first", Policy: sandbox.Shared}
	seedWorkspaceOwner(t, repo, spec.OwnerID, spec.ProjectID)
	workspace, err := provider.Acquire(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := workspace.ReadFile(ctx, "hello.txt"); err != nil || string(data) != "hello" {
		t.Fatalf("read: %q, %v", data, err)
	}
	if err := workspace.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := repo.Close(); err != nil {
		t.Fatal(err)
	}
	provider, err = New(config, dataClient, openStore(t, database).E2B)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []sandbox.Spec{{OwnerID: "bob", ProjectID: "web"}, {OwnerID: "alice", ProjectID: "other"}, {OwnerID: "alice"}} {
		if _, err := provider.OpenProject(ctx, other); !errors.Is(err, sandbox.ErrWorkspaceMissing) {
			t.Fatalf("workspace unexpectedly available for %+v: %v", other, err)
		}
	}
	spec.RunID, spec.SessionID = "next-run", "second"
	temporaryFailure.Store(true)
	if _, err := provider.Acquire(ctx, spec); err == nil {
		t.Fatal("expected an error while E2B is unavailable")
	}
	temporaryFailure.Store(false)
	workspace, err = provider.Acquire(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := workspace.ReadFile(ctx, "hello.txt"); err != nil || string(data) != "hello" {
		t.Fatalf("read after resume: %q, %v", data, err)
	}
	if err := workspace.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if created.Load() != 1 || connected.Load() != 1 || discovered.Load() != 1 {
		t.Fatalf("created %d, reconnected %d, resolved directory %d", created.Load(), connected.Load(), discovered.Load())
	}
}

func openStore(t *testing.T, database string) *sqlite.Database {
	t.Helper()
	store, err := sqlite.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func seedWorkspaceOwner(t *testing.T, store *sqlite.Database, owner, project string) {
	t.Helper()
	ctx := t.Context()
	if err := store.Accounts.CreatePasswordUser(ctx, auth.User{ID: owner, Email: owner + "@example.test"}, []byte("test-hash")); err != nil {
		t.Fatal(err)
	}
	if project != "" {
		if err := store.Projects.CreateProject(ctx, run.Project{OwnerID: owner, ID: project, Name: project}); err != nil {
			t.Fatal(err)
		}
	}
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSelfHostedUsesReturnedTrafficToken(t *testing.T) {
	var created, connected atomic.Bool
	var probes atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "self-hosted-key" {
			t.Error("missing self-hosted API key")
		}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v2/sandboxes":
			probes.Add(1)
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes":
			var body struct {
				Secure              *bool
				AutoPause           bool
				AllowInternetAccess json.RawMessage            `json:"allow_internet_access"`
				Network             map[string]json.RawMessage `json:"network"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Secure == nil || !*body.Secure || !body.AutoPause {
				t.Errorf("expected authenticated envd: %v", err)
			}
			if string(body.AllowInternetAccess) != "true" || len(body.Network) != 1 || string(body.Network["allowPublicTraffic"]) != "false" {
				t.Error("sandbox creation must request outbound internet and private ingress")
			}
			created.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, `{"sandboxID":"self-hosted-box","envdVersion":"0.11.0","trafficAccessToken":"private-token"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/sandboxes/self-hosted-box/connect":
			connected.Store(true)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"sandboxID":"self-hosted-box","envdVersion":"0.11.0"}`)
		case r.Method == http.MethodDelete:
			t.Error("the self-hosted sandbox was deleted")
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("E2b-Sandbox-Id") != "self-hosted-box" || r.Header.Get("E2b-Sandbox-Port") != "49983" ||
			r.Header.Get("E2b-Traffic-Access-Token") != "private-token" ||
			r.Header.Get("X-Access-Token") != "" || r.Header.Get("X-API-Key") != "" {
			t.Error("incorrect self-hosted data plane headers")
		}
		user, _, _ := r.BasicAuth()
		if user != "developer" || r.URL.Query().Get("path") != "/work/test.txt" {
			t.Error("custom workspace user or directory not used")
		}
		_, _ = io.WriteString(w, "self-hosted")
	}))
	defer proxy.Close()
	t.Setenv("E2B_DEBUG", "")

	store := openStore(t, ":memory:")
	seedWorkspaceOwner(t, store, "alice", "")
	provider, err := New(Config{APIKey: "self-hosted-key", BaseURL: api.URL, ProxyURL: proxy.URL,
		WorkspaceUser: "developer", WorkDirectory: "/work"}, proxy.Client(), store.E2B)
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{Policy: sandbox.Shared, RunID: "test-run", OwnerID: "alice"}
	box, err := provider.Acquire(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := box.ReadFile(t.Context(), "test.txt"); err != nil || string(data) != "self-hosted" {
		t.Fatalf("read via proxy: %q, %v", data, err)
	}
	if err := box.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	box, err = provider.OpenProject(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if data, err := box.ReadFile(t.Context(), "test.txt"); err != nil || string(data) != "self-hosted" {
		t.Fatalf("read resumed workspace via proxy: %q, %v", data, err)
	}
	if !created.Load() || !connected.Load() || probes.Load() != 1 {
		t.Fatal("configured control plane did not receive create and connect")
	}
}

func TestProjectWorkspaceOverridesFollowCurrentSettings(t *testing.T) {
	const box = `{"sandboxID":"fixture-box","envdVersion":"0.11.0","envdAccessToken":"token"}`
	const legacy = `{"sandboxID":"legacy-box","envdVersion":"0.11.0","envdAccessToken":"token"}`
	var discoveries atomic.Int32
	mux := http.NewServeMux()
	writeBox := func(w http.ResponseWriter, status int, body string) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		writeBox(w, http.StatusCreated, box)
	})
	mux.HandleFunc("POST /sandboxes/fixture-box/connect", func(w http.ResponseWriter, _ *http.Request) {
		writeBox(w, http.StatusOK, box)
	})
	mux.HandleFunc("POST /sandboxes/legacy-box/connect", func(w http.ResponseWriter, _ *http.Request) {
		writeBox(w, http.StatusOK, legacy)
	})
	mux.HandleFunc("GET /files", func(w http.ResponseWriter, r *http.Request) {
		user, _, _ := r.BasicAuth()
		_, _ = io.WriteString(w, user+":"+r.URL.Query().Get("path"))
	})
	mux.HandleFunc("POST /process.Process/Start", func(w http.ResponseWriter, r *http.Request) {
		request := readStart(t, r)
		if request.Process.Cmd != "sh" || request.Process.Cwd != "" {
			t.Errorf("unexpected working directory probe: %+v", request.Process)
		}
		discoveries.Add(1)
		user, _, _ := r.BasicAuth()
		dir := "/srv/template"
		if user != "" {
			dir = "/home/" + user
		}
		beginProcess(t, w)
		processOutput(t, w, "stdout", dir+"\n")
		finishProcess(t, w, 0)
	})
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	store := openStore(t, ":memory:")
	seedWorkspaceOwner(t, store, "owner", "project")
	config := Config{APIKey: "fixture", BaseURL: server.URL, ProxyURL: server.URL, APIVersion: "v1"}
	newProvider := func(user, dir string) *Provider {
		t.Helper()
		config := config
		config.WorkspaceUser, config.WorkDirectory = user, dir
		provider, err := New(config, server.Client(), store.E2B)
		if err != nil {
			t.Fatal(err)
		}
		return provider
	}
	ctx := t.Context()
	read := func(ws sandbox.Workspace, want string) {
		t.Helper()
		if data, err := ws.ReadFile(ctx, "file.txt"); err != nil || string(data) != want {
			t.Fatalf("workspace file request = %q, %v, want %q", data, err, want)
		}
		if err := ws.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}
	spec := sandbox.Spec{OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared}
	ws, err := newProvider("analyst", "/custom").Acquire(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	read(ws, "analyst:/custom/file.txt")
	reconnect := func(spec sandbox.Spec, user, dir, want string) {
		t.Helper()
		ws, err := newProvider(user, dir).OpenProject(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		read(ws, want)
	}
	reconnect(spec, "analyst", "", "analyst:/home/analyst/file.txt")
	reconnect(spec, "", "", ":/srv/template/file.txt")
	reconnect(spec, "", "/temporary", ":/temporary/file.txt")
	reconnect(spec, "", "", ":/srv/template/file.txt")
	if count := discoveries.Load(); count != 2 {
		t.Fatalf("got %d working directory probes, want one per user", count)
	}
	legacySpec := sandbox.Spec{OwnerID: spec.OwnerID, Policy: sandbox.Shared}
	provider := newProvider("", "")
	if saved, err := provider.saveProject(ctx, legacySpec, "", "legacy-box", credentials{EnvdToken: "token", Timeout: provider.config.Timeout}); err != nil || !saved {
		t.Fatalf("seed legacy workspace: saved = %t, error = %v", saved, err)
	}
	reconnect(legacySpec, "", "", "root:/root/file.txt")
	reconnect(legacySpec, "analyst", "/custom", "analyst:/custom/file.txt")
	reconnect(legacySpec, "", "", "root:/root/file.txt")
	if count := discoveries.Load(); count != 2 {
		t.Fatalf("legacy root and explicit directories should need no discovery, got %d probes", count)
	}
	// Records written before execution settings were tracked may contain the
	// old override as the effective location. Reconnect using the template.
	old := credentials{EnvdToken: "token", Timeout: provider.config.Timeout, WorkspaceUser: "analyst", WorkDirectory: "/custom"}
	encoded, err := json.Marshal(old)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.E2B.UpdateE2BCredentials(ctx, spec.OwnerID, spec.ProjectID, provider.endpoint, "fixture-box", encoded); err != nil {
		t.Fatal(err)
	}
	reconnect(spec, "", "", ":/srv/template/file.txt")
	reconnect(spec, "", "", ":/srv/template/file.txt")
	if count := discoveries.Load(); count != 3 {
		t.Fatalf("an older record should rediscover its template directory once, got %d total probes", count)
	}
}

func TestUncredentialedSandboxIsDeleted(t *testing.T) {
	var deleted atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sandboxID":"uncredentialed-box","envdVersion":"0.11.0"}`)
	})
	mux.HandleFunc("DELETE /sandboxes/uncredentialed-box", func(w http.ResponseWriter, _ *http.Request) {
		deleted.Store(true)
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	t.Setenv("E2B_DEBUG", "")

	provider, err := New(Config{APIKey: "fixture", BaseURL: server.URL}, server.Client(), openStore(t, ":memory:").E2B)
	if err != nil {
		t.Fatal(err)
	}
	if box, err := provider.Acquire(t.Context(), sandbox.Spec{Policy: sandbox.Shared, OwnerID: "alice"}); err == nil || box != nil {
		t.Fatal("accepted a workspace without data-plane credentials")
	}
	if !deleted.Load() {
		t.Fatal("rejected sandbox was not deleted")
	}
}
