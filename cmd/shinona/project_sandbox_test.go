package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

func TestRemovingProjectPreservesSandboxOnPreviousE2BControlPlane(t *testing.T) {
	var destroyed atomic.Bool
	previous := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/sandboxes/original-box" {
			t.Errorf("unexpected E2B request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		destroyed.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(previous.Close)
	app, db, query := settingsTestApp(t, settings.Values{
		settings.E2BAPIKey: "fixture", settings.E2BBaseURL: "https://current.example.test",
	})
	ctx := t.Context()
	project := run.Project{ID: "fixture-project", OwnerID: "alice", Name: "Fixture"}
	if err := db.Projects.CreateProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	if err := db.Projects.SaveProjectSandboxConfig(ctx, project.OwnerID, project.ID, run.ProjectSandboxConfig{Provider: sandbox.ProviderE2B}); err != nil {
		t.Fatal(err)
	}
	if saved, err := db.E2B.SaveE2BSandbox(ctx, project.OwnerID, project.ID, previous.URL, "", "original-box", []byte("{}")); err != nil || !saved {
		t.Fatalf("save original E2B binding: %t, %v", saved, err)
	}
	path := "/projects/" + project.ID
	if response := query(project.OwnerID, http.MethodDelete, path, nil); !strings.Contains(response, "PROJECT_SANDBOX_CLEANUP_REQUIRED") {
		t.Fatalf("delete project with an unreachable original control plane: %s", response)
	}
	if _, err := db.Projects.Project(ctx, project.OwnerID, project.ID); err != nil {
		t.Fatalf("project was lost when cleanup could not finish: %v", err)
	}
	if id, _, err := db.E2B.E2BSandbox(ctx, project.OwnerID, project.ID, previous.URL); err != nil || id != "original-box" || destroyed.Load() {
		t.Fatalf("original E2B workspace was not retained for retry: %q, %v", id, err)
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: settings.E2BBaseURL, Value: new(previous.URL)}}); err != nil {
		t.Fatal(err)
	}
	if response := query(project.OwnerID, http.MethodDelete, path, nil); response != "" || !destroyed.Load() {
		t.Fatalf("retry did not delete the project and its workspace: %s", response)
	}
	if _, err := db.Projects.Project(ctx, project.OwnerID, project.ID); !errors.Is(err, run.ErrProject) {
		t.Fatalf("deleted project still exists: %v", err)
	}
}

func TestProjectSandboxTimeoutOverridesReachProvisioning(t *testing.T) {
	type creation struct {
		TemplateID string
		Timeout    int
		AutoPause  bool
	}
	requests := make(chan creation, 4)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		var request creation
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case requests <- request:
		default:
			t.Error("E2B received unexpected extra create requests")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"sandboxID":"fixture-box","envdVersion":"0.11.0","envdAccessToken":"token"}`)
	})
	mux.HandleFunc("DELETE /sandboxes/fixture-box", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	remote := httptest.NewServer(mux)
	t.Cleanup(remote.Close)
	app, db, _ := settingsTestApp(t, settings.Values{
		settings.E2BAPIKey: "fixture", settings.E2BBaseURL: remote.URL, settings.E2BProxyURL: remote.URL,
		settings.E2BAPIVersion: "v1", settings.E2BWorkDirectory: "/workspace",
		settings.E2BTemplate: "instance-template", settings.E2BTimeout: "480", settings.E2BOnTimeout: sandbox.TimeoutPause,
	})
	controls := &run.Controls{}
	service := run.NewService(db.RunStores(), app.ResolveForAccount, uuid.NewString, controls, app.ProjectTargets, run.DefaultWorkspaceTimeouts())
	api := gin.New()
	rest.Register(api.Group(rest.Prefix), service, app, slog.New(slog.DiscardHandler), rest.DefaultConfig())
	const owner = "alice"
	ctx := auth.WithUser(t.Context(), auth.User{ID: owner})
	for _, tt := range []struct {
		name   string
		input  map[string]any
		create creation
	}{
		{name: "duration override", input: map[string]any{"template": "project-template", "timeoutSeconds": 90},
			create: creation{TemplateID: "project-template", Timeout: 90, AutoPause: true}},
		{name: "action override", input: map[string]any{"onTimeout": sandbox.TimeoutKill},
			create: creation{TemplateID: "instance-template", Timeout: 480}},
		{name: "global fallback", create: creation{TemplateID: "instance-template", Timeout: 480, AutoPause: true}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			project, err := service.CreateProject(ctx, owner, tt.name)
			if err != nil {
				t.Fatal(err)
			}
			path := "/projects/" + project.ID + "/sandbox"
			if tt.input != nil {
				tt.input["provider"] = sandbox.ProviderE2B
				response := performREST(t, api, ctx, http.MethodPut, path+"/config", map[string]any{"input": tt.input})
				if response.Code != http.StatusNoContent {
					t.Fatalf("save project sandbox config: %d %s", response.Code, response.Body)
				}
			}
			checkCreate := func(path string) {
				t.Helper()
				response := performREST(t, api, ctx, http.MethodPost, path, nil)
				if response.Code != http.StatusNoContent {
					t.Fatalf("provision project sandbox: %d %s", response.Code, response.Body)
				}
				select {
				case request := <-requests:
					if request != tt.create {
						t.Fatalf("E2B received %+v, want %+v", request, tt.create)
					}
				default:
					t.Fatal("E2B did not receive a create request")
				}
			}
			checkCreate(path)
			if tt.input != nil {
				checkCreate(path + "/recreate")
			}
		})
	}
}

func TestChangingE2BDurationKeepsTheProjectSandbox(t *testing.T) {
	type call struct {
		path    string
		timeout int
	}
	calls := make(chan call, 16)
	const box = `{"sandboxID":"fixture-box","envdVersion":"0.11.0","envdAccessToken":"token"}`
	mux := http.NewServeMux()
	record := func(w http.ResponseWriter, r *http.Request, response int) {
		var request struct{ Timeout int }
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode E2B request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		select {
		case calls <- call{r.URL.Path, request.Timeout}:
		default:
			t.Error("unexpected extra E2B request")
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response)
		if response != http.StatusNoContent {
			_, _ = io.WriteString(w, box)
		}
	}
	mux.HandleFunc("POST /sandboxes", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusCreated)
	})
	mux.HandleFunc("POST /sandboxes/fixture-box/connect", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusOK)
	})
	mux.HandleFunc("POST /sandboxes/fixture-box/timeout", func(w http.ResponseWriter, r *http.Request) {
		record(w, r, http.StatusNoContent)
	})
	mux.HandleFunc("GET /sandboxes/fixture-box", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"sandboxID":"fixture-box","state":"running","lifecycle":{"onTimeout":"pause","autoResume":false}}`)
	})
	mux.HandleFunc("DELETE /sandboxes/fixture-box", func(w http.ResponseWriter, _ *http.Request) {
		t.Error("changing the duration destroyed the active sandbox")
		w.WriteHeader(http.StatusNoContent)
	})
	remote := httptest.NewServer(mux)
	t.Cleanup(remote.Close)
	app, db, _ := settingsTestApp(t, settings.Values{
		settings.E2BAPIKey: "fixture", settings.E2BBaseURL: remote.URL, settings.E2BProxyURL: remote.URL,
		settings.E2BAPIVersion: "v1", settings.E2BWorkDirectory: "/workspace", settings.E2BTimeout: "480",
	})
	service := run.NewService(db.RunStores(), app.ResolveForAccount, uuid.NewString, &run.Controls{}, app.ProjectTargets, run.DefaultWorkspaceTimeouts())
	const owner = "alice"
	ctx := t.Context()
	if err := service.CreateProjectSandbox(ctx, owner, ""); err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(ctx, owner, "")
	if err != nil {
		t.Fatal(err)
	}
	open := func() {
		t.Helper()
		if err := service.WithWorkspace(ctx, owner, session.ID, func(context.Context, sandbox.Workspace) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.SaveProjectSandbox(ctx, owner, "", run.ProjectSandboxConfig{Provider: sandbox.ProviderE2B, TimeoutSeconds: 90}, ""); err != nil {
		t.Fatal(err)
	}
	open()
	open()
	if _, err := app.Update(ctx, []settings.Change{{Key: settings.E2BTimeout, Value: new("30")}}); err != nil {
		t.Fatal(err)
	}
	if err := service.SaveProjectSandbox(ctx, owner, "", run.ProjectSandboxConfig{Provider: sandbox.ProviderE2B}, ""); err != nil {
		t.Fatal(err)
	}
	open()
	close(calls)
	var got []call
	for call := range calls {
		got = append(got, call)
	}
	want := []call{
		{"/sandboxes", 480},
		{"/sandboxes/fixture-box/connect", 90}, {"/sandboxes/fixture-box/timeout", 90},
		{"/sandboxes/fixture-box/connect", 90},
		{"/sandboxes/fixture-box/connect", 30}, {"/sandboxes/fixture-box/timeout", 30},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("E2B requests = %v, want %v", got, want)
	}

	if _, err := app.Update(ctx, []settings.Change{{Key: settings.E2BOnTimeout, Value: new(sandbox.TimeoutKill)}}); err != nil {
		t.Fatal(err)
	}
	api := gin.New()
	rest.Register(api.Group(rest.Prefix), service, app, slog.New(slog.DiscardHandler), rest.DefaultConfig())
	response := performREST(t, api, auth.WithUser(ctx, auth.User{ID: owner}), http.MethodGet, "/projects/default/sandbox", nil)
	var status struct {
		ActiveOnTimeout string
		Effective       struct{ OnTimeout string }
	}
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &status) != nil ||
		status.ActiveOnTimeout != sandbox.TimeoutPause || status.Effective.OnTimeout != sandbox.TimeoutKill {
		t.Fatalf("incorrect configured or active E2B action: %d %s", response.Code, response.Body)
	}
}
