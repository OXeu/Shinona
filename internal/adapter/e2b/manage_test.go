package e2b

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestProjectLifecycleUsesProjectTemplateAndTimeout(t *testing.T) {
	var exists atomic.Bool
	var deletes atomic.Int32
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
		_, _ = io.WriteString(w, `{"sandboxID":"project-box","envdVersion":"0.11.0","trafficAccessToken":"private","envdAccessToken":"envd"}`)
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
	}, server.Client(), store)
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	spec := sandbox.Spec{
		RunID: "create", OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared,
		Template: "project-template", Timeout: 90 * time.Second, OnTimeout: sandbox.TimeoutKill,
	}
	ws, err := provider.Acquire(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatal(err)
	}
	status, err := provider.ProjectStatus(ctx, spec)
	if err != nil || status.ID != "project-box" || status.State != sandbox.ProjectPaused {
		t.Fatalf("paused status: %+v, %v", status, err)
	}
	if err := provider.DestroyProject(ctx, spec); err != nil {
		t.Fatal(err)
	}
	if err := provider.DestroyProject(ctx, spec); err != nil {
		t.Fatal(err)
	}
	status, err = provider.ProjectStatus(ctx, spec)
	if err != nil || status.ID != "" || status.State != sandbox.ProjectNone || deletes.Load() != 1 {
		t.Fatalf("destroyed status: %+v, %v, %d deletes", status, err, deletes.Load())
	}
}
