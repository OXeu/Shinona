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

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/demo"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

func settingsTestApp(t *testing.T, legacy settings.Values) (*runtime, *sqlite.Database, func(string, string, string, any) string) {
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
		if saved, err := repo.Settings.SaveSettingOverrides(t.Context(), 0, legacy); err != nil || !saved {
			t.Fatalf("seed instance overrides: %t, %v", saved, err)
		}
	}
	for _, id := range []string{"alice", "bob"} {
		if err := repo.Accounts.CreatePasswordUser(t.Context(), auth.User{ID: id, Email: id + "@example.test"}, []byte("test-hash")); err != nil {
			t.Fatal(err)
		}
	}
	app, err := newRuntime(t.Context(), repo.Settings, repo.E2B, nil)
	if err != nil {
		t.Fatal(err)
	}
	service := run.NewService(repo.RunStores(), app.ResolveForAccount, uuid.NewString, &run.Controls{}, app.ProjectTargets)
	api := gin.New()
	rest.Register(api.Group(rest.Prefix), service, app, slog.New(slog.DiscardHandler), rest.DefaultTranscriptConcurrency())
	query := func(owner, method, path string, body any) string {
		t.Helper()
		ctx := auth.WithUser(t.Context(), auth.User{ID: owner})
		return performREST(t, api, ctx, method, path, body).Body.String()
	}
	return app, repo, query
}

func performREST(t *testing.T, api http.Handler, ctx context.Context, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		encoded = data
	}
	request := httptest.NewRequestWithContext(ctx, method, rest.Prefix+path, bytes.NewReader(encoded))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	return response
}

type modelTransport func(*http.Request) (*http.Response, error)

func (f modelTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestModelSettingsKeepTrustedOriginsWithWrappedTransport(t *testing.T) {
	const trusted = "https://127.0.0.1:43210"
	t.Setenv("SHINONA_TRUSTED_MODEL_ORIGINS", trusted)
	app, repo, query := settingsTestApp(t, nil)
	app.modelRuntime.client.Transport = modelTransport(app.modelRuntime.client.Transport.RoundTrip)
	save := func(base string) string {
		return query("alice", http.MethodPatch, "/settings/model", map[string]any{
			"changes": []map[string]string{{"key": "modelBaseUrl", "value": base}},
		})
	}
	if response := save(trusted + "/v1"); !strings.Contains(response, trusted+"/v1") {
		t.Fatalf("saving the trusted origin failed with a wrapped transport: %s", response)
	}
	if response := save("https://127.0.0.1:43211/v1"); !strings.Contains(response, "PRIVATE_ENDPOINT") {
		t.Fatalf("a different private origin was accepted: %s", response)
	}
	if values, err := repo.Settings.ModelSettings(t.Context(), "alice"); err != nil || values[settings.ModelBaseURL] != trusted+"/v1" {
		t.Fatalf("failed validation changed the stored base URL: %v", err)
	}
}

func TestAccountModelSettingsReachComposerAndWorker(t *testing.T) {
	t.Setenv("SHINONA_MODEL_API_KEY", "legacy-env-key")
	app, repo, query := settingsTestApp(t, settings.Values{
		settings.ModelBaseURL: "https://legacy.example.test/v1", settings.ModelAPIKey: "legacy-db-key", settings.ModelID: "legacy-model",
	})
	completions := make(chan string, 2)
	app.modelRuntime.client = &http.Client{Transport: modelTransport(func(req *http.Request) (*http.Response, error) {
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
	const modelPath = "/settings/model"
	const catalogPath = "/settings/model/catalog"
	for _, owner := range []string{"alice", "bob"} {
		before := query(owner, http.MethodGet, "/composer/options", nil) + query(owner, http.MethodGet, modelPath, nil)
		if !strings.Contains(before, `"defaultModel":""`) || strings.Contains(before, "legacy") {
			t.Fatalf("global settings leaked into private config: %s", before)
		}
		changes := map[string]any{"changes": []map[string]string{
			{"key": "modelBaseUrl", "value": "https://models.example.test/v1"},
			{"key": "modelApiKey", "value": owner + "-key"},
			{"key": "modelId", "value": owner + "-key-model"},
		}}
		if result := query(owner, http.MethodPatch, modelPath, changes); !strings.Contains(result, `"key":"modelApiKey","value":null,"configured":true`) {
			t.Fatalf("account settings did not save or leaked the key: %s", result)
		}
		if result := query(owner, http.MethodPost, catalogPath, map[string]any{}); !strings.Contains(result, owner+"-key-model") {
			t.Fatalf("catalog did not use caller's key: %s", result)
		}
	}
	if result := query("alice", http.MethodGet, "/composer/options", nil); !strings.Contains(result, `"contextWindow":10000`) || strings.Contains(result, "bob") {
		t.Fatalf("composer catalog is not isolated: %s", result)
	}
	if result := query("alice", http.MethodPost, catalogPath, map[string]string{"baseUrl": "https://new.example.test/v1"}); result != "[]" {
		t.Fatalf("model discovery at a new endpoint failed: %s", result)
	}
	if result := query("alice", http.MethodPatch, modelPath, map[string]any{"changes": []map[string]string{{"key": "e2bApiKey", "value": "invalid"}}}); !strings.Contains(result, "INVALID_SETTING_CHANGES") {
		t.Fatalf("account settings accepted an instance field: %s", result)
	}

	resolve := func(context.Context, string, string) (run.Target, error) {
		return run.Target{Name: "demo", Provider: demo.Provider{}, ResolveAgent: app.resolveAgent}, nil
	}
	controls := &run.Controls{}
	service := run.NewService(repo.RunStores(), resolve, uuid.NewString, controls, nil)
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
		run.NewWorker(repo.Conversations, repo.Projects, resolve, slog.New(slog.DiscardHandler), controls, run.DefaultWorkerConcurrency).Serve(workerCtx)
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
	if result := query("alice", http.MethodPatch, modelPath, map[string]any{"changes": []map[string]string{{"key": "modelBaseUrl", "value": "https://new.example.test/v1"}}}); !strings.Contains(result, `"key":"modelApiKey","value":null,"configured":false`) {
		t.Fatalf("changing the base URL retained its previous key: %s", result)
	}
	if result := query("bob", http.MethodGet, modelPath, nil); !strings.Contains(result, `"key":"modelApiKey","value":null,"configured":true`) {
		t.Fatalf("editing alice's settings changed bob's: %s", result)
	}
}
