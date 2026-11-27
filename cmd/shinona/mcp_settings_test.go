package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/OXeu/shinona/internal/adapter/mcp"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
)

func TestMCPSettingsAndComposerBatchGrantReads(t *testing.T) {
	app, repo, _ := settingsTestApp(t, nil)
	ctx := auth.WithUser(t.Context(), auth.User{ID: "alice"})
	config := settings.MCPConfig{
		{Name: "first", URL: "https://first.example.test/mcp", OAuth: &settings.MCPOAuthConfig{}},
		{Name: "second", URL: "https://second.example.test/mcp", OAuth: &settings.MCPOAuthConfig{}},
		{Name: "public", URL: "https://public.example.test/mcp"},
	}
	if err := repo.Settings.SaveMCPConfig(ctx, "alice", config, nil); err != nil {
		t.Fatal(err)
	}
	read, recorder := timing.WithRecorder(ctx)
	view, err := app.MCPSettings(read)
	if err != nil || len(view.Servers) != 3 {
		t.Fatalf("read MCP settings: %+v, %v", view, err)
	}
	checkQueries := func(want int) {
		t.Helper()
		metrics := recorder.Metrics()
		if i := slices.IndexFunc(metrics, func(metric timing.Metric) bool { return metric.Name == "sqlite" }); i < 0 || metrics[i].Calls != want {
			t.Fatalf("SQLite calls should be %d independently of server count: %v", want, metrics)
		}
	}
	checkQueries(2) // Account configuration and one grant query.
	read, recorder = timing.WithRecorder(ctx)
	options, err := app.ComposerOptions(read)
	if err != nil || !slices.Equal(options.MCPServers, []string{"public"}) {
		t.Fatalf("read composer MCP servers: %v, %v", options.MCPServers, err)
	}
	checkQueries(3) // Model configuration, MCP configuration and one grant query.

	failure := errors.New("fixture SQLite failure")
	app.oauth = mcp.NewOAuth(failingMCPGrantRead{OAuthStore: repo.Settings, err: failure}, "http://127.0.0.1:5173")
	if _, err := app.MCPSettings(ctx); !errors.Is(err, failure) {
		t.Fatalf("settings hid grant lookup failure: %v", err)
	}
	if _, err := app.ComposerOptions(ctx); !errors.Is(err, failure) {
		t.Fatalf("composer hid grant lookup failure: %v", err)
	}
	if _, err := app.RemoveMCPServer(ctx, "public"); !errors.Is(err, failure) {
		t.Fatalf("settings mutation hid grant lookup failure: %v", err)
	}
	saved, err := repo.Settings.MCPConfig(ctx, "alice")
	if err != nil || len(saved) != len(config) {
		t.Fatalf("failed response committed a settings change: %+v, %v", saved, err)
	}
}

type failingMCPGrantRead struct {
	mcp.OAuthStore
	err error
}

func (f failingMCPGrantRead) MCPGrants(context.Context, string, []string) (map[string][]byte, error) {
	return nil, f.err
}

func TestMCPOAuthLookupFailureReturnsServerError(t *testing.T) {
	app, repo, _ := settingsTestApp(t, nil)
	routes := http.NewServeMux()
	requireUser := func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(auth.WithUser(r.Context(), auth.User{ID: "alice"})))
		})
	}
	app.registerOAuth(routes, requireUser, false, slog.New(slog.DiscardHandler))
	for _, path := range []string{"/auth/mcp/start", "/auth/mcp/disconnect"} {
		app.mcpRuntime.repo = repo.Settings
		request := func() *httptest.ResponseRecorder {
			req := httptest.NewRequestWithContext(t.Context(), http.MethodPost, path, strings.NewReader(`{"name":"missing"}`))
			req.Header.Set("Content-Type", "application/json")
			resp := httptest.NewRecorder()
			routes.ServeHTTP(resp, req)
			return resp
		}
		if resp := request(); resp.Code != http.StatusNotFound {
			t.Fatalf("%s missing server: %d %s", path, resp.Code, resp.Body.String())
		}
		app.mcpRuntime.repo = failingMCPConfigRead{MCPRepository: repo.Settings}
		if resp := request(); resp.Code != http.StatusInternalServerError || strings.Contains(resp.Body.String(), "fixture DB failure") {
			t.Fatalf("%s lookup failure: %d %s", path, resp.Code, resp.Body.String())
		}
	}
}

type failingMCPConfigRead struct{ settings.MCPRepository }

func (failingMCPConfigRead) MCPConfig(context.Context, string) (settings.MCPConfig, error) {
	return nil, errors.New("fixture DB failure")
}

func TestMCPSettingsUseRegisteredOAuthClient(t *testing.T) {
	app, repo, query := settingsTestApp(t, nil)
	ctx := t.Context()
	const owner, name, endpoint = "alice", "registry", "https://mcp.example.test/tools"
	const secret = "fixture-client-secret"
	input := map[string]any{"name": name, "url": endpoint, "oauth": true, "oauthClientId": "registered", "oauthClientSecret": secret}
	assertClient := func(id, storedSecret string) {
		t.Helper()
		response := query(owner, http.MethodPut, "/settings/mcp/servers", input)
		var view settings.MCPSettings
		if err := json.Unmarshal([]byte(response), &view); err != nil || len(view.Servers) != 1 || strings.Contains(response, secret) {
			t.Fatal("saving MCP settings failed or exposed the registered secret")
		}
		info := view.Servers[0]
		if !info.OAuth || info.OAuthClientID != id || info.HasOAuthClientSecret != (storedSecret != "") {
			t.Fatalf("unexpected public OAuth client metadata: %+v", info)
		}
		server, ok, err := app.lookupMCP(ctx, owner, name)
		if err != nil || !ok || server.OAuth == nil || server.OAuth.ClientID != id || server.OAuth.ClientSecret != storedSecret {
			t.Fatal("OAuth discovery did not receive the saved client configuration")
		}
	}
	assertClient("registered", secret)
	if response := query(owner, http.MethodGet, "/settings/mcp", nil); strings.Contains(response, secret) ||
		!strings.Contains(response, `"oauthClientId":"registered"`) {
		t.Fatal("loading MCP settings did not report redacted client metadata")
	}
	if err := repo.Settings.SaveMCPGrant(ctx, owner, name, []byte("grant-fixture")); err != nil {
		t.Fatal(err)
	}
	input["previousName"] = name
	delete(input, "oauthClientSecret")
	assertClient("registered", secret)
	if data, err := repo.Settings.MCPGrant(ctx, owner, name); err != nil || string(data) != "grant-fixture" {
		t.Fatalf("saving unchanged credentials removed the grant: %v", err)
	}

	input["oauthClientId"] = "different-client"
	assertClient("different-client", "")
	if data, err := repo.Settings.MCPGrant(ctx, owner, name); err != nil || len(data) != 0 {
		t.Fatalf("changing client credentials retained the old grant: %v", err)
	}
	input["oauthClientSecret"] = secret
	assertClient("different-client", secret)
	input["oauthClientSecret"] = ""
	assertClient("different-client", "")

	input["oauthClientSecret"] = secret
	assertClient("different-client", secret)
	delete(input, "oauthClientSecret")
	input["url"] = "https://other.example.test/tools"
	assertClient("different-client", "")

	input["oauthClientId"] = ""
	assertClient("", "")
	input["oauthClientSecret"] = secret
	if response := query(owner, http.MethodPut, "/settings/mcp/servers", input); !strings.Contains(response, "INVALID_MCP_OAUTH_CLIENT") ||
		strings.Contains(response, secret) {
		t.Fatal("saving a client secret without an ID did not return a redacted validation error")
	}
}

func TestMCPSettingsBelongToTheAccount(t *testing.T) {
	t.Setenv("SHINONA_MCP_SERVERS", `[{"name":"startup","url":"https://example.test/mcp","token":"startup-secret"}]`)
	app, repo, query := settingsTestApp(t, nil)
	ctx := t.Context()
	const mcpPath = "/settings/mcp"
	const serversPath = mcpPath + "/servers"
	read := func(owner string) string {
		return query(owner, http.MethodGet, mcpPath, nil) + query(owner, http.MethodGet, "/composer/options", nil)
	}
	if response := read("alice"); !strings.Contains(response, `"servers":[]`) || strings.Contains(response, "startup") {
		t.Fatalf("account initialized with shared MCP settings: %s", response)
	}
	input := map[string]any{"name": "shared-name", "url": "https://alice.example.test/mcp", "token": "alice-secret"}
	if response := query("alice", http.MethodPut, serversPath, input); !strings.Contains(response, `"hasToken":true`) || strings.Contains(response, "alice-secret") {
		t.Fatalf("personal MCP write did not mask its credential: %s", response)
	}
	if response := read("bob"); !strings.Contains(response, `"servers":[]`) || strings.Contains(response, "alice") {
		t.Fatalf("second account saw first account's MCP config: %s", response)
	}
	input["url"], input["token"] = "https://bob.example.test/mcp", "bob-secret"
	if response := query("bob", http.MethodPut, serversPath, input); !strings.Contains(response, "bob.example.test") || strings.Contains(response, "bob-secret") {
		t.Fatalf("second account could not reuse the name: %s", response)
	}
	if response := read("alice"); !strings.Contains(response, `"mcpServers":["shared-name"]`) || strings.Contains(response, "bob.example.test") {
		t.Fatalf("composer did not use the current account's MCP settings: %s", response)
	}
	delete(input, "token")
	input["previousName"], input["name"], input["url"] = "shared-name", "renamed", "https://alice.example.test/mcp"
	if response := query("alice", http.MethodPut, serversPath, input); !strings.Contains(response, `"name":"renamed"`) || !strings.Contains(response, `"hasToken":true`) {
		t.Fatalf("renaming lost the account's token: %s", response)
	}
	reloaded, err := newRuntime(ctx, repo.Settings, repo.E2B, nil, run.DefaultWorkspaceTimeouts().Cleanup)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := reloaded.accountMCP(ctx, "alice")
	if err != nil || len(stored) != 1 || stored[0].Name != "renamed" || stored[0].Token != "alice-secret" {
		t.Fatalf("account's MCP settings did not survive reload: %v", err)
	}
	if server, ok, err := app.lookupMCP(ctx, "bob", "renamed"); err != nil || ok || server.URL != "" {
		t.Fatal("MCP OAuth could resolve another account's server")
	}
	input["previousName"], input["url"] = "renamed", "https://another.example.test/mcp"
	if response := query("alice", http.MethodPut, serversPath, input); !strings.Contains(response, "MCP_TOKEN_REQUIRED_FOR_URL_CHANGE") {
		t.Fatalf("saved token could be carried to another endpoint: %s", response)
	}
	input["token"], input["url"] = "", "http://127.0.0.1:8383/mcp"
	if response := query("alice", http.MethodPut, serversPath, input); !strings.Contains(response, "PRIVATE_ENDPOINT") {
		t.Fatalf("user MCP accepted a loopback endpoint: %s", response)
	}
	input["url"], input["oauth"], input["name"] = "https://alice.example.test/mcp", true, "shared-name"
	if response := query("alice", http.MethodPut, serversPath, input); !strings.Contains(response, `"oauth":true`) {
		t.Fatalf("cannot save OAuth without client credentials: %s", response)
	}
	input["url"], input["previousName"] = "https://bob.example.test/mcp", "shared-name"
	if response := query("bob", http.MethodPut, serversPath, input); !strings.Contains(response, `"oauth":true`) {
		t.Fatalf("cannot save OAuth for second account: %s", response)
	}
	for _, owner := range []string{"alice", "bob"} {
		if err := repo.Settings.SaveMCPGrant(ctx, owner, "shared-name", []byte("grant-fixture")); err != nil {
			t.Fatal(err)
		}
	}
	if response := query("alice", http.MethodDelete, mcpPath, nil); !strings.Contains(response, `"servers":[]`) {
		t.Fatalf("clear personal MCP settings failed: %s", response)
	}
	aliceGrant, err := repo.Settings.MCPGrant(ctx, "alice", "shared-name")
	if err != nil || aliceGrant != nil {
		t.Fatalf("removing MCP settings retained the same account's grant: %v", err)
	}
	bobGrant, err := repo.Settings.MCPGrant(ctx, "bob", "shared-name")
	if err != nil || string(bobGrant) != "grant-fixture" {
		t.Fatalf("removing MCP settings deleted another account's grant: %v", err)
	}
	if response := read("bob"); !strings.Contains(response, "bob.example.test") {
		t.Fatalf("clearing alice's config changed bob's config: %s", response)
	}
	legacy, err := repo.Settings.SettingOverrides(ctx)
	if err != nil || legacy.Values[settings.MCPServers] != "" {
		t.Fatalf("account MCP writes changed instance settings: %v", err)
	}
}
