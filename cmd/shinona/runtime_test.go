package main

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

type pausedRuntimeSettingsCommit struct {
	*sqlite.SettingsStore
	committed chan struct{}
	publish   chan struct{}
}

func (repo *pausedRuntimeSettingsCommit) SaveSettingOverrides(ctx context.Context, revision int64, values settings.Values) (bool, error) {
	saved, err := repo.SettingsStore.SaveSettingOverrides(ctx, revision, values)
	if saved && revision == 0 {
		close(repo.committed)
		<-repo.publish
	}
	return saved, err
}

func TestInstanceSettingsRebaseWhileEarlierCommitAwaitsPublication(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	repo := &pausedRuntimeSettingsCommit{SettingsStore: db.Settings, committed: make(chan struct{}), publish: make(chan struct{})}
	app, err := newRuntime(ctx, repo, db.E2B, nil, run.DefaultWorkspaceTimeouts().Cleanup)
	if err != nil {
		t.Fatal(err)
	}
	firstDone := make(chan error, 1)
	go func() {
		_, err := app.Update(ctx, []settings.Change{{Key: settings.MailFrom, Value: new("one@example.test")}})
		firstDone <- err
	}()
	release := sync.OnceFunc(func() { close(repo.publish) })
	defer release()
	select {
	case <-repo.committed:
	case <-ctx.Done():
		t.Fatal("the first write did not commit")
	}
	if app.current.Load().revision != 0 {
		t.Fatal("test did not pause the first publication")
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: settings.E2BTemplate, Value: new("second-template")}}); err != nil {
		t.Fatalf("later update could not proceed while an earlier publication was paused: %v", err)
	}
	release()
	select {
	case err := <-firstDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("the earlier update did not finish")
	}
	stored, err := db.Settings.SettingOverrides(ctx)
	current := app.current.Load()
	if err != nil || current.revision != 2 || stored.Revision != 2 ||
		stored.Values[settings.MailFrom] != "one@example.test" || current.values[settings.MailFrom] != "one@example.test" ||
		stored.Values[settings.E2BTemplate] != "second-template" || current.e2b.Defaults.Template != "second-template" {
		t.Fatalf("later publication did not preserve both committed changes: %v", err)
	}
}

func TestDotEnvFallbackAndSavedOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	content := "SHINONA_E2B_TEMPLATE=dotenv-template\nSHINONA_E2B_DOMAIN=dotenv.example\nSHINONA_WEB_ORIGIN=http://127.0.0.1:5174\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SHINONA_E2B_DOMAIN", "process.example")
	unsetTestEnv(t, "SHINONA_E2B_TEMPLATE")
	unsetTestEnv(t, "SHINONA_WEB_ORIGIN")
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
	first, err := newRuntime(t.Context(), repository.Settings, repository.E2B, environment, run.DefaultWorkspaceTimeouts().Cleanup)
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
	reloaded, err := newRuntime(t.Context(), repository.Settings, repository.E2B, environment, run.DefaultWorkspaceTimeouts().Cleanup)
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
	if saved, err := repository.Settings.SaveSettingOverrides(ctx, 0, obsolete); err != nil || !saved {
		t.Fatalf("seed obsolete instance overrides: %t, %v", saved, err)
	}
	app, err := newRuntime(ctx, repository.Settings, repository.E2B, nil, run.DefaultWorkspaceTimeouts().Cleanup)
	if err != nil {
		t.Fatal(err)
	}
	if overrides, err := repository.Settings.SettingOverrides(ctx); err != nil || len(overrides.Values) != 0 {
		t.Fatalf("obsolete instance overrides survived startup: %v", err)
	}
	service := run.NewService(repository.RunStores(), app.ResolveForAccount, uuid.NewString, &run.Controls{}, nil, run.DefaultWorkspaceTimeouts())
	api := gin.New()
	rest.Register(api.Group(rest.Prefix), service, app, slog.New(slog.DiscardHandler), rest.DefaultConfig())
	const path = "/settings/application"
	as := func(admin bool, method string, body any) string {
		t.Helper()
		user := auth.WithUser(ctx, auth.User{ID: "owner", Admin: admin})
		return performREST(t, api, user, method, path, body).Body.String()
	}
	if response := as(false, http.MethodGet, nil); response != "[]" {
		t.Fatalf("settings exposed to a regular user: %s", response)
	}
	adminFields := as(true, http.MethodGet, nil)
	for key := range obsolete {
		if strings.Contains(adminFields, `"key":"`+string(key)+`"`) {
			t.Fatalf("obsolete setting is still listed: %s", key)
		}
		if _, err := app.Update(ctx, []settings.Change{{Key: key, Value: new("test")}}); err == nil {
			t.Fatalf("obsolete setting still accepts updates: %s", key)
		}
	}
	changes := map[string]any{"changes": []map[string]string{
		{"key": "e2bBaseUrl", "value": "http://127.0.0.1:3000"},
		{"key": "e2bApiKey", "value": "private-e2b-key"},
	}}
	if response := as(false, http.MethodPatch, changes); !strings.Contains(response, "administrator access is required") || app.Resolve("").Provider != nil {
		t.Fatalf("regular user was allowed to save settings: %s", response)
	}
	saved := as(true, http.MethodPatch, changes)
	if strings.Contains(saved, "private-") ||
		!strings.Contains(saved, `"key":"e2bApiKey","value":null,"configured":true`) {
		t.Fatalf("unexpected settings mutation result: %s", saved)
	}
	owner := auth.WithUser(ctx, auth.User{ID: "owner"})
	if err := repository.Accounts.CreatePasswordUser(ctx, auth.User{ID: "owner", Email: "owner@example.test"}, []byte("test-hash")); err != nil {
		t.Fatal(err)
	}
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
	item, err := repository.Conversations.ClaimNext(ctx, session.UpdatedAt)
	if err != nil || item == nil || item.Backend != "e2b" || item.Policy != sandbox.Shared || app.Resolve(item.Backend).Provider == nil {
		t.Fatalf("queued turn lost its E2B provider: %+v, %v", item, err)
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: "envdBrokerUrl", Value: new("http://127.0.0.1:4800")}}); err == nil {
		t.Fatal("obsolete broker setting was accepted")
	}
	if _, err := app.Update(ctx, []settings.Change{{Key: "SHINONA_ADDR", Value: new("127.0.0.1:9999")}}); err == nil {
		t.Fatal("bootstrap setting was accepted")
	}
	persisted, err := repository.Settings.SettingOverrides(ctx)
	if err != nil || persisted.Values["envdBrokerUrl"] != "" || persisted.Values[settings.E2BBaseURL] != "http://127.0.0.1:3000" {
		t.Fatalf("provider settings lost or partially written: %v", err)
	}
}
