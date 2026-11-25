package main

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/graphql"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

func TestDotEnvFallbackAndSavedOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "SHINONA_E2B_TEMPLATE=dotenv-template\nSHINONA_E2B_DOMAIN=dotenv.example\nSHINONA_WEB_ORIGIN=http://127.0.0.1:5174\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHINONA_E2B_DOMAIN", "process.example")
	t.Setenv("SHINONA_E2B_TEMPLATE", "")
	t.Setenv("SHINONA_WEB_ORIGIN", "")
	environment, err := readEnvironment(path)
	if err != nil {
		t.Fatal(err)
	}
	defaults := settings.Defaults(environment.lookup)
	if defaults[settings.E2BTemplate] != "dotenv-template" || defaults[settings.E2BDomain] != "process.example" {
		t.Fatalf("unexpected startup values: template %q, domain %q", defaults[settings.E2BTemplate], defaults[settings.E2BDomain])
	}
	repository, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	first, err := newRuntime(t.Context(), repository, environment)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Update(t.Context(), []settings.Change{
		{Key: settings.E2BTemplate, Value: new("saved-template")},
		{Key: settings.E2BDomain, Value: new("")},
	}); err != nil {
		t.Fatal(err)
	}
	environment["SHINONA_E2B_TEMPLATE"] = "updated-template"
	reloaded, err := newRuntime(t.Context(), repository, environment)
	if err != nil {
		t.Fatal(err)
	}
	if values := reloaded.current.Load().values; values[settings.E2BTemplate] != "saved-template" || values[settings.E2BDomain] != "" {
		t.Fatalf("saved settings did not win over startup fallbacks")
	}
	if _, err := reloaded.Update(t.Context(), []settings.Change{{Key: settings.E2BTemplate}}); err != nil {
		t.Fatal(err)
	}
	if got := reloaded.current.Load().values[settings.E2BTemplate]; got != "updated-template" {
		t.Fatalf("reset did not restore startup value: %q", got)
	}
	if reloaded.Host() != "127.0.0.1:5174" || reloaded.Auth().Secure {
		t.Fatalf("login did not use the deployment origin from dotenv: %q", reloaded.Host())
	}
}

func TestRuntimeSettingsAuthorizeRedactAndSwitchProviders(t *testing.T) {
	ctx := t.Context()
	repository, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repository.Close(); err != nil {
			t.Error(err)
		}
	})
	obsolete := settings.Values{
		"envdUrl": "http://127.0.0.1:49983", "envdToken": "private-legacy-token",
		"webOrigin": "https://old.example", "projectName": "Old project", "machineName": "Old machine",
		"projectBranch": "main", "projectRemoteUrl": "invalid legacy URL",
	}
	if err := repository.SaveSettingOverrides(ctx, obsolete); err != nil {
		t.Fatal(err)
	}
	app, err := newRuntime(ctx, repository, nil)
	if err != nil {
		t.Fatal(err)
	}
	if overrides, err := repository.SettingOverrides(ctx); err != nil || len(overrides) != 0 {
		t.Fatalf("obsolete instance overrides survived startup: %v", err)
	}
	service := run.NewService(repository, app.Resolve, uuid.NewString, &run.Controls{})
	api, err := graphql.NewHandler(service, app, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}

	as := func(admin bool, query string, variables any) string {
		t.Helper()
		body, err := json.Marshal(map[string]any{"query": query, "variables": variables})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/graphql", bytes.NewReader(body)).
			WithContext(auth.WithUser(ctx, auth.User{ID: "owner", Admin: admin}))
		response := httptest.NewRecorder()
		api.ServeHTTP(response, request)
		return response.Body.String()
	}
	const read = `{ applicationSettings { key configured value } }`
	if response := as(false, read, nil); !strings.Contains(response, `"applicationSettings":[]`) {
		t.Fatalf("settings exposed to a regular user: %s", response)
	}
	adminFields := as(true, read, nil)
	for key := range obsolete {
		if strings.Contains(adminFields, `"key":"`+string(key)+`"`) {
			t.Fatalf("obsolete setting is still listed: %s", key)
		}
		if _, err := app.Update(ctx, []settings.Change{{Key: key, Value: new("test")}}); err == nil {
			t.Fatalf("obsolete setting still accepts updates: %s", key)
		}
	}
	const update = `mutation Update($changes: [SettingChange!]!) {
		updateApplicationSettings(changes: $changes) { key configured value }
	}`
	changes := map[string]any{"changes": []map[string]string{
		{"key": "e2bBaseUrl", "value": "http://127.0.0.1:3000"},
		{"key": "e2bApiKey", "value": "private-e2b-key"},
	}}
	if response := as(false, update, changes); !strings.Contains(response, "administrator access is required") || app.Resolve("").Provider != nil {
		t.Fatalf("regular user was allowed to save settings: %s", response)
	}
	saved := as(true, update, changes)
	if strings.Contains(saved, "errors") || strings.Contains(saved, "private-") ||
		!strings.Contains(saved, `"key":"e2bApiKey","configured":true,"value":null`) {
		t.Fatalf("unexpected settings mutation result: %s", saved)
	}
	owner := auth.WithUser(ctx, auth.User{ID: "owner"})
	if _, err := app.UpdateModelSettings(owner, []settings.Change{
		{Key: settings.ModelBaseURL, Value: new("https://api.openai.com/v1")},
		{Key: settings.ModelID, Value: new("gpt-4o-mini")},
		{Key: settings.ModelAPIKey, Value: new("private-model-key")},
	}); err != nil {
		t.Fatal(err)
	}
	session, err := service.CreateSession(ctx, "owner", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Submit(ctx, "owner", session.ID, "Stored provider", run.Payload{}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: "mode", Value: new("envd")}}); err == nil {
		t.Fatal("obsolete envd mode was accepted")
	}
	if policies := app.Resolve("").Provider.Policies("owner"); len(policies) != 1 || policies[0] != sandbox.Shared {
		t.Fatalf("E2B policies: %v", policies)
	}
	item, err := repository.ClaimNext(ctx, session.UpdatedAt)
	if err != nil || item == nil || item.Backend != "e2b" || item.Policy != sandbox.Shared || app.Resolve(item.Backend).Provider == nil {
		t.Fatalf("queued turn lost its E2B provider: %+v, %v", item, err)
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: "envdBrokerUrl", Value: new("http://127.0.0.1:4800")}}); err == nil {
		t.Fatal("obsolete broker setting was accepted")
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: "SHINONA_ADDR", Value: new("127.0.0.1:9999")}}); err == nil {
		t.Fatal("bootstrap setting was accepted")
	}
	persisted, err := repository.SettingOverrides(ctx)
	if err != nil || persisted["envdBrokerUrl"] != "" || persisted[settings.E2BBaseURL] != "http://127.0.0.1:3000" {
		t.Fatalf("provider settings lost or partially written: %v", err)
	}
}
