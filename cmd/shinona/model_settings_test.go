package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/graphql"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

func settingsTestApp(t *testing.T, legacy settings.Values) (*runtime, *sqlite.Repository, func(string, string, map[string]any) string) {
	t.Helper()
	repo, err := sqlite.Open(t.Context(), ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	if legacy != nil {
		if err := repo.SaveSettingOverrides(t.Context(), legacy); err != nil {
			t.Fatal(err)
		}
	}
	app, err := newRuntime(t.Context(), repo, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := run.NewService(repo, app.Resolve, uuid.NewString, &run.Controls{})
	api, err := graphql.NewHandler(service, app, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	query := func(owner, document string, vars map[string]any) string {
		t.Helper()
		body, err := json.Marshal(map[string]any{"query": document, "variables": vars})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequestWithContext(auth.WithUser(t.Context(), auth.User{ID: owner}),
			http.MethodPost, "/graphql", bytes.NewReader(body))
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		return response.Body.String()
	}
	return app, repo, query
}

type modelTransport func(*http.Request) (*http.Response, error)

func (f modelTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestAccountModelSettingsReachComposerAndWorker(t *testing.T) {
	t.Setenv("SHINONA_MODEL_API_KEY", "legacy-env-key")
	app, repo, query := settingsTestApp(t, settings.Values{
		settings.ModelBaseURL: "https://legacy.example.test/v1", settings.ModelAPIKey: "legacy-db-key", settings.ModelID: "legacy-model",
	})
	completions := make(chan string, 2)
	app.models.client = &http.Client{Transport: modelTransport(func(req *http.Request) (*http.Response, error) {
		owner := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
		var body string
		switch {
		case req.URL.Hostname() == "new.example.test":
			if owner != "" {
				t.Error("stored key was sent to a different model endpoint")
			}
			body = `{"data":[]}`
		case owner != "alice-key" && owner != "bob-key":
			t.Error("model request used the wrong account's credentials")
			body = `{"data":[]}`
		case req.URL.Path == "/v1/models":
			body = fmt.Sprintf(`{"data":[{"id":%q,"context_length":10000}]}`, owner+"-model")
		case req.URL.Path == "/v1/chat/completions":
			var input struct{ Model string }
			if err := json.NewDecoder(req.Body).Decode(&input); err != nil || input.Model != owner+"-model" {
				t.Errorf("wrong model for account credential: %v, %s", err, input.Model)
			}
			completions <- owner
			body = `{"choices":[{"message":{"role":"assistant","content":"complete"},"finish_reason":"stop"}]}`
		default:
			t.Error("unexpected endpoint path")
			body = `{"data":[]}`
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
	})}
	const fields = "{ key value configured }"
	const read = "{ modelSettings " + fields + " composerOptions { defaultModel models { id contextWindow } } }"
	const update = "mutation ($changes:[SettingChange!]!) { updateModelSettings(changes:$changes) " + fields + " }"
	for _, owner := range []string{"alice", "bob"} {
		before := query(owner, read, nil)
		if !strings.Contains(before, `"defaultModel":""`) || strings.Contains(before, "legacy") {
			t.Fatalf("global settings leaked into private config: %s", before)
		}
		changes := map[string]any{"changes": []map[string]string{
			{"key": "modelBaseUrl", "value": "https://models.example.test/v1"},
			{"key": "modelApiKey", "value": owner + "-key"},
			{"key": "modelId", "value": owner + "-key-model"},
		}}
		if result := query(owner, update, changes); strings.Contains(result, "errors") ||
			!strings.Contains(result, `"key":"modelApiKey","value":null,"configured":true`) {
			t.Fatalf("account settings did not save or leaked the key: %s", result)
		}
		if result := query(owner, "mutation { fetchModels { id } }", nil); !strings.Contains(result, owner+"-key-model") {
			t.Fatalf("catalog did not use caller's key: %s", result)
		}
	}
	if result := query("alice", read, nil); !strings.Contains(result, `"contextWindow":10000`) || strings.Contains(result, "bob") {
		t.Fatalf("composer catalog is not isolated: %s", result)
	}
	if result := query("alice", `mutation { fetchModels(baseUrl:"https://new.example.test/v1") { id } }`, nil); strings.Contains(result, "errors") {
		t.Fatalf("model discovery at a new endpoint failed: %s", result)
	}
	if result := query("alice", `mutation { updateModelSettings(changes:[{key:"e2bApiKey",value:"invalid"}]) { key } }`, nil); !strings.Contains(result, "INVALID_SETTING_CHANGES") {
		t.Fatalf("account settings accepted an instance field: %s", result)
	}

	resolve := func(string) run.Target {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: app.resolveAgent}
	}
	controls := &run.Controls{}
	service := run.NewService(repo, resolve, uuid.NewString, controls)
	for _, owner := range []string{"alice", "bob"} {
		session, err := service.CreateSession(t.Context(), owner, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Submit(t.Context(), owner, session.ID, "test model credentials", run.Payload{}, false); err != nil {
			t.Fatal(err)
		}
	}
	workerCtx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		run.NewWorker(repo, resolve, slog.New(slog.DiscardHandler), controls).Serve(workerCtx)
	}()
	defer func() { cancel(); <-done }()
	executed := map[string]bool{}
	timeout := time.After(5 * time.Second)
	for range 2 {
		select {
		case owner := <-completions:
			executed[owner] = true
		case <-timeout:
			t.Fatal("both accounts' queued runs did not reach their model endpoints")
		}
	}
	if !executed["alice-key"] || !executed["bob-key"] {
		t.Fatalf("queued runs used the same account's model key: %v", executed)
	}
	if result := query("alice", `mutation { updateModelSettings(changes:[{key:"modelBaseUrl",value:"https://new.example.test/v1"}]) { key configured } }`, nil); !strings.Contains(result, `"key":"modelApiKey","configured":false`) {
		t.Fatalf("changing the base URL retained its previous key: %s", result)
	}
	if result := query("bob", read, nil); !strings.Contains(result, `"key":"modelApiKey","value":null,"configured":true`) {
		t.Fatalf("editing alice's settings changed bob's: %s", result)
	}
}
