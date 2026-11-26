package main

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

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
	service := run.NewService(db.RunStores(), app.ResolveForAccount, uuid.NewString, controls, app.ProjectTargets)
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
