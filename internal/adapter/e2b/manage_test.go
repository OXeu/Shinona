package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestProjectLifecycleUsesProjectTemplateAndTimeout(t *testing.T) {
	var exists atomic.Bool
	var deletes atomic.Int32
	var connects atomic.Int32
	const workspaceResponse = `{"sandboxID":"project-box","envdVersion":"0.11.0","trafficAccessToken":"private","envdAccessToken":"envd"}`
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			TemplateID string
			Timeout    int
			AutoPause  bool
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TemplateID != "project-template" ||
			body.Timeout != 90 || body.AutoPause {
			t.Errorf("incorrect project template or timeout policy: %+v, %v", body, err)
		}
		exists.Store(true)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, workspaceResponse)
	})
	mux.HandleFunc("POST /sandboxes/project-box/connect", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Timeout int }
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Timeout != 90 {
			t.Errorf("incorrect project timeout while opening existing sandbox: %+v, %v", body, err)
		}
		connects.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, workspaceResponse)
	})
	mux.HandleFunc("GET /sandboxes/project-box", func(w http.ResponseWriter, _ *http.Request) {
		if !exists.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxID":"project-box","state":"paused"}`)
	})
	mux.HandleFunc("DELETE /sandboxes/project-box", func(w http.ResponseWriter, _ *http.Request) {
		deletes.Add(1)
		exists.Store(false)
		w.WriteHeader(http.StatusNoContent)
	})
	server := httptest.NewServer(mux)
	defer server.Close()
	store := openStore(t, ":memory:")
	seedWorkspaceOwner(t, store, "owner", "project")
	provider, err := New(Config{
		APIKey: "fixture", BaseURL: server.URL, ProxyURL: server.URL,
		Template: "instance-template", Timeout: 480 * time.Second, OnTimeout: sandbox.TimeoutPause,
		WorkDirectory: "/workspace",
	}, server.Client(), store.E2B)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	spec := sandbox.Spec{
		RunID: "create", OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared,
		Template: "project-template",
	}
	providerWithPolicy := provider.WithIdleTimeout(sandbox.IdleTimeout{After: 90 * time.Second, Action: sandbox.TimeoutKill})
	ws, err := providerWithPolicy.Acquire(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatal(err)
	}
	ws, err = providerWithPolicy.OpenProject(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if connects.Load() != 1 {
		t.Fatalf("opening the project made %d connect requests", connects.Load())
	}
	status, err := provider.ProjectStatus(ctx, spec)
	if err != nil || status.ID != "project-box" || status.State != sandbox.ProjectPaused {
		t.Fatalf("paused status: %+v, %v", status, err)
	}
	for _, expected := range []string{"previous-box", ""} {
		if err := provider.DestroyProject(ctx, spec, &expected); !errors.Is(err, sandbox.ErrProjectChanged) || deletes.Load() != 0 {
			t.Fatalf("destroy with a stale ID %q: %v, %d deletes", expected, err, deletes.Load())
		}
	}
	if err := provider.DestroyProject(ctx, spec, &status.ID); err != nil {
		t.Fatal(err)
	}
	if err := provider.DestroyProject(ctx, spec, nil); err != nil {
		t.Fatal(err)
	}
	status, err = provider.ProjectStatus(ctx, spec)
	if err != nil || status.ID != "" || status.State != sandbox.ProjectNone || deletes.Load() != 1 {
		t.Fatalf("destroyed status: %+v, %v, %d deletes", status, err, deletes.Load())
	}
}

func TestDestroyProjectRetriesBindingCleanupAfterRemoteDeletion(t *testing.T) {
	const owner, project, id = "owner", "project", "retained-box"
	const endpoint = "https://e2b.example.test"
	const cleanupTimeout = 100 * time.Millisecond
	database := openStore(t, ":memory:")
	seedWorkspaceOwner(t, database, owner, project)
	ctx := t.Context()
	if saved, err := database.E2B.SaveE2BSandbox(ctx, owner, project, endpoint, "", id, []byte("{}")); err != nil || !saved {
		t.Fatalf("seed retained sandbox: %t, %v", saved, err)
	}
	request, cancel := context.WithCancel(ctx)
	defer cancel()
	var kills int
	client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodDelete || req.URL.Path != "/sandboxes/"+id {
			t.Errorf("unexpected E2B request: %s %s", req.Method, req.URL)
		}
		kills++
		status := http.StatusNotFound
		if kills == 1 {
			status = http.StatusNoContent
			// Model an acknowledged deletion whose caller has disconnected.
			cancel()
		}
		return &http.Response{StatusCode: status, Body: http.NoBody, Header: make(http.Header)}, nil
	})}
	var writes int
	store := deleteBindingStore{WorkspaceStore: database.E2B, beforeDelete: func(cleanup context.Context) error {
		writes++
		deadline, ok := cleanup.Deadline()
		if remaining := time.Until(deadline); !ok || remaining > cleanupTimeout {
			t.Fatalf("binding cleanup has %v remaining, want a deadline within %v", remaining, cleanupTimeout)
		}
		if writes == 1 {
			if !errors.Is(request.Err(), context.Canceled) {
				t.Error("fixture did not cancel the original request")
			}
			// The write can be blocked by another SQLite request until the
			// cleanup deadline. Keep the binding available for the retry.
			<-cleanup.Done()
			return cleanup.Err()
		}
		return nil
	}}
	provider, err := New(Config{APIKey: "fixture", BaseURL: endpoint, CleanupTimeout: cleanupTimeout}, client, store)
	if err != nil {
		t.Fatal(err)
	}
	spec := sandbox.Spec{OwnerID: owner, ProjectID: project}
	expectedID := id
	if err := provider.DestroyProject(request, spec, &expectedID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("destroy did not report the binding cleanup deadline: %v", err)
	}
	if retained, _, err := database.E2B.E2BSandbox(ctx, owner, project, endpoint); err != nil || retained != id {
		t.Fatalf("failed cleanup lost the retained binding %q: %v", retained, err)
	}
	if err := provider.DestroyProject(ctx, spec, &expectedID); err != nil {
		t.Fatalf("retry after remote deletion: %v", err)
	}
	if retained, _, err := database.E2B.E2BSandbox(ctx, owner, project, endpoint); err != nil || retained != "" || kills != 2 || writes != 2 {
		t.Fatalf("retry left binding %q after %d E2B requests and %d writes: %v", retained, kills, writes, err)
	}
}

func TestDestroyProjectReconcilesAmbiguousDeletion(t *testing.T) {
	const owner, project, id = "owner", "project", "retained-box"
	const endpoint = "https://e2b.example.test"
	const cleanupTimeout = time.Second
	for _, tc := range []struct {
		name         string
		deleteStatus int
		cancelDelete bool
		checkStatus  int
		removed      bool
	}{
		{name: "lost reply after client cancellation", cancelDelete: true, checkStatus: http.StatusNotFound, removed: true},
		{name: "gateway failed after deleting", deleteStatus: http.StatusBadGateway, checkStatus: http.StatusGone, removed: true},
		{name: "request timed out with sandbox still paused", deleteStatus: http.StatusRequestTimeout, checkStatus: http.StatusOK},
		{name: "control plane still unavailable", deleteStatus: http.StatusBadGateway, checkStatus: http.StatusServiceUnavailable},
		{name: "credentials rejected", deleteStatus: http.StatusUnauthorized},
	} {
		t.Run(tc.name, func(t *testing.T) {
			database := openStore(t, ":memory:")
			seedWorkspaceOwner(t, database, owner, project)
			ctx := t.Context()
			if saved, err := database.E2B.SaveE2BSandbox(ctx, owner, project, endpoint, "", id, []byte("{}")); err != nil || !saved {
				t.Fatalf("seed retained sandbox: %t, %v", saved, err)
			}
			request, cancel := context.WithCancel(ctx)
			defer cancel()
			var deletes, checks int
			client := &http.Client{Transport: transportFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/sandboxes/"+id {
					t.Fatalf("unexpected E2B request: %s %s", req.Method, req.URL)
				}
				response := &http.Response{Body: http.NoBody, Header: make(http.Header)}
				// Model the final SDK result without waiting for its retry backoff.
				response.Header.Set("Should-Retry", "false")
				switch req.Method {
				case http.MethodDelete:
					deletes++
					if tc.cancelDelete {
						cancel()
						return nil, context.Canceled
					}
					response.StatusCode = tc.deleteStatus
				case http.MethodGet:
					checks++
					if deletes != 1 || tc.checkStatus == 0 || req.Context().Err() != nil {
						t.Fatalf("unexpected deletion check after %d attempts: %v", deletes, req.Context().Err())
					}
					deadline, ok := req.Context().Deadline()
					if !ok || time.Until(deadline) > cleanupTimeout {
						t.Fatalf("deletion check must use the configured cleanup deadline, got %v", deadline)
					}
					response.StatusCode = tc.checkStatus
					if tc.checkStatus == http.StatusOK {
						response.Header.Set("Content-Type", "application/json")
						response.Body = io.NopCloser(strings.NewReader(`{"sandboxID":"retained-box","state":"paused"}`))
					}
				default:
					t.Fatalf("unexpected E2B method %s", req.Method)
				}
				return response, nil
			})}
			provider, err := New(Config{APIKey: "fixture", BaseURL: endpoint, CleanupTimeout: cleanupTimeout}, client, database.E2B)
			if err != nil {
				t.Fatal(err)
			}
			expectedID := id
			err = provider.DestroyProject(request, sandbox.Spec{OwnerID: owner, ProjectID: project}, &expectedID)
			retained, _, readErr := database.E2B.E2BSandbox(ctx, owner, project, endpoint)
			if tc.removed {
				if err != nil || retained != "" {
					t.Fatalf("confirmed deletion left binding %q: %v", retained, err)
				}
			} else if !hasStatus(err, tc.deleteStatus) || retained != id {
				t.Fatalf("unconfirmed deletion changed binding %q or lost the original error: %v", retained, err)
			}
			if tc.checkStatus == http.StatusServiceUnavailable && !strings.Contains(err.Error(), "verify E2B sandbox deletion: e2b: http 503") {
				t.Fatalf("verification failure was lost: %v", err)
			}
			if readErr != nil || deletes != 1 || checks > 1 || (checks == 1) != (tc.checkStatus != 0) {
				t.Fatalf("binding lookup or request count: %v, %d deletes, %d checks", readErr, deletes, checks)
			}
		})
	}
}

type deleteBindingStore struct {
	WorkspaceStore
	beforeDelete func(context.Context) error
}

func (s deleteBindingStore) DeleteE2BSandbox(ctx context.Context, owner, project, endpoint, id string) error {
	if err := s.beforeDelete(ctx); err != nil {
		return err
	}
	return s.WorkspaceStore.DeleteE2BSandbox(ctx, owner, project, endpoint, id)
}
