package e2b

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/sandbox"
)

func TestProviderReconnectsProjectSandboxAfterRestart(t *testing.T) {
	var created, connected atomic.Int32
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
		if r.Header.Get("X-Access-Token") != token || r.Header.Get("E2b-Traffic-Access-Token") != "traffic-token" ||
			r.Header.Get("X-API-Key") != "" ||
			r.Header.Get("E2b-Sandbox-Id") != "test-box" || r.URL.Query().Get("path") != "/home/user/hello.txt" {
			t.Error("unexpected envd data-plane request")
		}
		_, _ = io.WriteString(w, "hello")
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
	config := Config{APIKey: "cloud-key", Template: "base"}
	provider, err := New(config, dataClient, repo)
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{RunID: "run-id", OwnerID: "alice", ProjectID: "web", SessionID: "first", Policy: sandbox.Shared, TTL: 140 * time.Second}
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
	provider, err = New(config, dataClient, openStore(t, database))
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
	if created.Load() != 1 || connected.Load() != 1 {
		t.Fatalf("created %d, reconnected %d", created.Load(), connected.Load())
	}
}

func openStore(t *testing.T, database string) *sqlite.Repository {
	t.Helper()
	store, err := sqlite.Open(t.Context(), database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

type transportFunc func(*http.Request) (*http.Response, error)

func (f transportFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestSelfHostedUsesReturnedTrafficToken(t *testing.T) {
	var created, connected atomic.Bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "self-hosted-key" {
			t.Error("missing self-hosted API key")
		}
		switch {
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
		_, _ = io.WriteString(w, "self-hosted")
	}))
	defer proxy.Close()
	t.Setenv("E2B_DEBUG", "")

	provider, err := New(Config{APIKey: "self-hosted-key", BaseURL: api.URL, ProxyURL: proxy.URL}, proxy.Client(), openStore(t, ":memory:"))
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
	if !created.Load() || !connected.Load() {
		t.Fatal("configured control plane did not receive create and connect")
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

	provider, err := New(Config{APIKey: "fixture", BaseURL: server.URL}, server.Client(), openStore(t, ":memory:"))
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
