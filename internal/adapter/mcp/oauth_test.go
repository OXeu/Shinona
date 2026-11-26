package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/settings"
)

func TestOAuthToolAccessAndRefresh(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "shinona.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, id := range []string{"owner", "other"} {
		if err := store.CreatePasswordUser(ctx, auth.User{ID: id, Email: id + "@example.test"}, []byte("test-hash")); err != nil {
			t.Fatal(err)
		}
	}
	remote := sdk.NewServer(&sdk.Implementation{Name: "fixture", Version: "1"}, nil)
	sdk.AddTool(remote, &sdk.Tool{Name: "echo"}, func(_ context.Context, _ *sdk.CallToolRequest, input struct {
		Text string `json:"text"`
	}) (*sdk.CallToolResult, any, error) {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: input.Text}}}, nil, nil
	})
	remoteHTTP := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return remote }, nil)
	const origin = "http://127.0.0.1:5174"
	var fixture *httptest.Server
	var verifierChallenge string
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer fresh-access" {
			remoteHTTP.ServeHTTP(w, r)
			return
		}
		w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+fixture.URL+`/resource-metadata", scope="tools"`)
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/resource-metadata", func(w http.ResponseWriter, _ *http.Request) {
		writeOAuthJSON(w, map[string]any{"resource": fixture.URL + "/mcp", "authorization_servers": []string{fixture.URL}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, _ *http.Request) {
		writeOAuthJSON(w, map[string]any{
			"issuer": fixture.URL, "authorization_endpoint": fixture.URL + "/authorize", "token_endpoint": fixture.URL + "/token",
			"registration_endpoint":            fixture.URL + "/register",
			"code_challenge_methods_supported": []string{"S256"}, "authorization_response_iss_parameter_supported": true,
		})
	})
	mux.HandleFunc("POST /register", func(w http.ResponseWriter, r *http.Request) {
		var registration oauthex.ClientRegistrationMetadata
		if err := json.NewDecoder(r.Body).Decode(&registration); err != nil || len(registration.RedirectURIs) != 1 ||
			registration.RedirectURIs[0] != origin+OAuthCallback || registration.TokenEndpointAuthMethod != "none" ||
			registration.ApplicationType != "native" {
			t.Errorf("unexpected dynamic client registration: %+v (%v)", registration, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusCreated)
		writeOAuthJSON(w, map[string]any{"client_id": "fixture-client", "token_endpoint_auth_method": "none"})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("resource") != fixture.URL+"/mcp" || r.Form.Get("client_id") != "fixture-client" {
			t.Errorf("token request is missing its resource or client: %v, %v", r.Form, err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var access, refresh string
		switch r.Form.Get("grant_type") {
		case "authorization_code":
			if oauth2.S256ChallengeFromVerifier(r.Form.Get("code_verifier")) != verifierChallenge ||
				r.Form.Get("redirect_uri") != origin+OAuthCallback || r.Form.Get("code") != "fixture-code" {
				t.Error("code exchange failed PKCE, code, or redirect validation")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			access, refresh = "original-access", "original-refresh"
		case "refresh_token":
			if r.Form.Get("refresh_token") == "rotated-refresh" {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				writeOAuthJSON(w, map[string]string{"error": "invalid_grant"})
				return
			}
			if r.Form.Get("refresh_token") != "original-refresh" {
				t.Error("refresh request did not use the original grant")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			access, refresh = "fresh-access", "rotated-refresh"
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		writeOAuthJSON(w, map[string]any{"access_token": access, "refresh_token": refresh, "token_type": "Bearer", "expires_in": 3600})
	})
	fixture = httptest.NewServer(mux)
	t.Cleanup(fixture.Close)
	server := settings.MCPServer{Name: "fixture", URL: fixture.URL + "/mcp", OAuth: &settings.MCPOAuthConfig{}}
	lookup := func(name string) (settings.MCPServer, bool) { return server, name == server.Name }
	oauth := NewOAuth(store, origin)
	state, destination, err := oauth.Start(ctx, "owner", server, "")
	if err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(destination)
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	verifierChallenge = query.Get("code_challenge")
	if verifierChallenge == "" || query.Get("code_challenge_method") != "S256" || query.Get("scope") != "tools" ||
		query.Get("resource") != server.URL || query.Get("state") != state || query.Get("client_id") != "fixture-client" {
		t.Fatalf("unexpected authorization parameters: %v", query)
	}
	if err := oauth.Finish(ctx, "other", state, "fixture-code", fixture.URL, lookup); !errors.Is(err, ErrOAuthFlow) {
		t.Fatalf("another account redeemed the state: %v", err)
	}
	if err := oauth.ApplyServerChanges(ctx, "other", func() ([]string, error) { return []string{server.Name}, nil }); err != nil {
		t.Fatal(err)
	}
	if err := oauth.Finish(ctx, "owner", state, "fixture-code", fixture.URL, lookup); err != nil {
		t.Fatalf("another account's settings change invalidated the flow: %v", err)
	}
	if err := oauth.Finish(ctx, "owner", state, "fixture-code", fixture.URL, lookup); !errors.Is(err, ErrOAuthFlow) {
		t.Fatalf("state replay was accepted: %v", err)
	}
	grant, _, err := oauth.grant(ctx, "owner", server)
	if err != nil {
		t.Fatal(err)
	}
	grant.Token.Expiry = time.Now().Add(-time.Minute)
	data, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMCPGrant(ctx, "owner", server.Name, data); err != nil {
		t.Fatal(err)
	}
	// A new OAuth instance reads the grant from SQLite after a restart.
	oauth = NewOAuth(store, origin)
	servers := NewServers(settings.MCPConfig{server}, fixture.Client(), oauth)
	if len(servers.Names(ctx, "owner")) != 1 || len(servers.Names(ctx, "other")) != 0 {
		t.Fatal("tool availability must follow the requesting account")
	}
	if _, _, err := servers.Open(ctx, "other", []string{server.Name}); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("unconnected account opened OAuth tools: %v", err)
	}
	tools, closeTools, err := servers.Open(ctx, "owner", []string{server.Name})
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != qualifiedToolName("fixture", "echo") {
		_ = closeTools()
		t.Fatalf("unexpected tools: %v", tools)
	}
	result, err := tools[0].Execute(ctx, map[string]any{"text": "hello"})
	_ = closeTools()
	if err != nil || result.Text != "hello" {
		t.Fatalf("call tool: %+v, %v", result, err)
	}
	grant, _, err = oauth.grant(ctx, "owner", server)
	if err != nil || grant.Token.RefreshToken != "rotated-refresh" {
		t.Fatalf("the refreshed grant was not saved: %v", err)
	}
	grant.Token.Expiry = time.Now().Add(-time.Minute)
	revoked, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveMCPGrant(ctx, "owner", server.Name, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := oauth.Token(ctx, "owner", server); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("invalidated refresh token was accepted: %v", err)
	}
	if data, err := store.MCPGrant(ctx, "owner", server.Name); err != nil || len(data) != 0 {
		t.Fatalf("revoked grant was not removed: %v", err)
	}
	if err := oauth.Disconnect(ctx, "owner", server.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := oauth.Token(ctx, "owner", server); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("disconnected account obtained a token: %v", err)
	}
}

func TestClientMetadataDocumentIsUsedBeforeDynamicRegistration(t *testing.T) {
	const origin = "https://shinona.example.test"
	oauth := NewOAuth(nil, origin)
	document := oauth.ClientMetadata()
	if document["client_id"] != origin+OAuthMetadata {
		t.Fatalf("unexpected client document: %v", document)
	}
	clientID, secret, style, err := oauthClient(t.Context(), nil, &oauthex.AuthServerMeta{
		ClientIDMetadataDocumentSupported: true, RegistrationEndpoint: "https://auth.example.test/register",
	}, nil, oauth.redirect, oauth.clientMetadata, false)
	if err != nil || clientID != document["client_id"] || secret != "" || style != oauth2.AuthStyleInParams {
		t.Fatalf("could not use client metadata document: %s, %v", clientID, err)
	}
}

func TestConfiguredOAuthClientTakesPrecedence(t *testing.T) {
	metadata := &oauthex.AuthServerMeta{
		ClientIDMetadataDocumentSupported: true,
		TokenEndpointAuthMethodsSupported: []string{"client_secret_post"},
	}
	configured := &settings.MCPOAuthConfig{ClientID: "registered", ClientSecret: "fixture"}
	clientID, secret, style, err := oauthClient(t.Context(), nil, metadata, configured,
		"https://shinona.example/auth/mcp/callback", "https://shinona.example/auth/mcp/client.json", false)
	if err != nil || clientID != configured.ClientID || secret != configured.ClientSecret || style != oauth2.AuthStyleInParams {
		t.Fatalf("configured client was not selected: %q, %v", clientID, err)
	}
}
