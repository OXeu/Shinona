package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
		if err := store.Accounts.CreatePasswordUser(ctx, auth.User{ID: id, Email: id + "@example.test"}, []byte("test-hash")); err != nil {
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
	lookup := func(_ context.Context, owner, name string) (settings.MCPServer, bool) {
		return server, owner == "owner" && name == server.Name
	}
	oauth := NewOAuth(store.Settings, origin)
	state, destination, err := oauth.Start(ctx, "owner", server.Name, "", lookup)
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
	if err := store.Settings.SaveMCPGrant(ctx, "owner", server.Name, data); err != nil {
		t.Fatal(err)
	}
	// A new OAuth instance reads the grant from SQLite after a restart.
	oauth = NewOAuth(store.Settings, origin)
	changed := server
	changed.Name, changed.URL = "changed", server.URL+"/new"
	stale := changed
	stale.URL = server.URL
	grant.Fingerprint = fingerprint(stale)
	staleGrant, err := json.Marshal(grant)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Settings.SaveMCPGrant(ctx, "owner", changed.Name, staleGrant); err != nil {
		t.Fatal(err)
	}
	servers := NewServers(settings.MCPConfig{server, changed}, fixture.Client(), oauth)
	if names, err := servers.Names(ctx, "owner"); err != nil || len(names) != 1 || names[0] != server.Name {
		t.Fatalf("tool availability did not exclude the stale grant: %v, %v", names, err)
	}
	if names, err := servers.Names(ctx, "other"); err != nil || len(names) != 0 {
		t.Fatalf("tool availability did not follow the requesting account: %v, %v", names, err)
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
	if err := store.Settings.SaveMCPGrant(ctx, "owner", server.Name, revoked); err != nil {
		t.Fatal(err)
	}
	if _, err := oauth.Token(ctx, "owner", server); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("invalidated refresh token was accepted: %v", err)
	}
	if data, err := store.Settings.MCPGrant(ctx, "owner", server.Name); err != nil || len(data) != 0 {
		t.Fatalf("revoked grant was not removed: %v", err)
	}
	if err := oauth.Disconnect(ctx, "owner", server.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := oauth.Token(ctx, "owner", server); !errors.Is(err, ErrOAuthRequired) {
		t.Fatalf("disconnected account obtained a token: %v", err)
	}
}

func TestOAuthRefreshDuringCallerOrGrantChanges(t *testing.T) {
	for _, scenario := range []string{"cancel caller", "disconnect grant", "replace grant"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "shinona.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			const owner = "owner"
			if err := store.Accounts.CreatePasswordUser(ctx, auth.User{ID: owner, Email: "owner@example.test"}, []byte("test-hash")); err != nil {
				t.Fatal(err)
			}
			started, reply := make(chan struct{}), make(chan struct{})
			var refreshes atomic.Int32
			fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != "original-refresh" {
					t.Error("unexpected refresh request")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if refreshes.Add(1) != 1 {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusBadRequest)
					writeOAuthJSON(w, map[string]string{"error": "invalid_grant"})
					return
				}
				close(started)
				select {
				case <-reply:
					writeOAuthJSON(w, map[string]any{"access_token": "rotated-access", "refresh_token": "rotated-refresh", "expires_in": 3600})
				case <-r.Context().Done():
				}
			}))
			t.Cleanup(fixture.Close)
			allowReply := sync.OnceFunc(func() { close(reply) })
			t.Cleanup(allowReply)
			server := settings.MCPServer{Name: "fixture", URL: fixture.URL + "/mcp", OAuth: &settings.MCPOAuthConfig{}}
			oauth := NewOAuth(store.Settings, "http://127.0.0.1:5174")
			grant := oauthGrant{
				Fingerprint: fingerprint(server), Resource: server.URL,
				Config: oauth2.Config{ClientID: "fixture", RedirectURL: oauth.redirect, Endpoint: oauth2.Endpoint{TokenURL: fixture.URL + "/token"}},
			}
			save := func(token *oauth2.Token) {
				t.Helper()
				grant.Token = token
				data, err := json.Marshal(grant)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Settings.SaveMCPGrant(ctx, owner, server.Name, data); err != nil {
					t.Fatal(err)
				}
			}
			save(&oauth2.Token{AccessToken: "original-access", RefreshToken: "original-refresh", Expiry: time.Now().Add(-time.Minute)})
			type tokenResult struct {
				token *oauth2.Token
				err   error
			}
			caller, cancelCaller := context.WithCancel(ctx)
			defer cancelCaller()
			result := make(chan tokenResult, 1)
			go func() {
				token, err := oauth.Token(caller, owner, server)
				result <- tokenResult{token, err}
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("refresh did not reach the issuer")
			}
			await := func() tokenResult {
				t.Helper()
				select {
				case result := <-result:
					return result
				case <-ctx.Done():
					t.Fatal("refresh caller did not return")
					return tokenResult{}
				}
			}
			wantAccess, wantRefresh := "rotated-access", "rotated-refresh"
			switch scenario {
			case "cancel caller":
				cancelCaller()
				if got := await(); !errors.Is(got.err, context.Canceled) || got.token != nil {
					t.Errorf("cancelled caller should return immediately: %v", got.err)
				}
				allowReply()
				if token, err := oauth.Token(ctx, owner, server); err != nil || token.AccessToken != wantAccess {
					t.Errorf("next caller did not obtain the rotated token: %v", err)
				}
			case "disconnect grant":
				if err := oauth.Disconnect(ctx, owner, server.Name); err != nil {
					t.Fatal(err)
				}
				allowReply()
				if got := await(); !errors.Is(got.err, ErrOAuthRequired) || got.token != nil {
					t.Errorf("refresh returned a disconnected grant: %v", got.err)
				}
				wantAccess, wantRefresh = "", ""
			case "replace grant":
				wantAccess, wantRefresh = "new-access", "new-refresh"
				save(&oauth2.Token{AccessToken: wantAccess, RefreshToken: wantRefresh, Expiry: time.Now().Add(time.Hour)})
				if token, err := oauth.Token(ctx, owner, server); err != nil || token.AccessToken != wantAccess {
					t.Errorf("new authorization waited for the old refresh: %v", err)
				}
				allowReply()
				if got := await(); got.err != nil || got.token.AccessToken != wantAccess {
					t.Errorf("refresh did not return the newer authorization: %v", got.err)
				}
			}
			if refreshes.Load() != 1 {
				t.Errorf("the issuer received %d requests for one refresh token", refreshes.Load())
			}
			saved, _, err := oauth.grant(ctx, owner, server)
			if wantAccess == "" {
				if !errors.Is(err, ErrOAuthRequired) {
					t.Fatalf("refresh restored a disconnected grant: %v", err)
				}
			} else if err != nil || saved.Token.AccessToken != wantAccess || saved.Token.RefreshToken != wantRefresh {
				t.Fatalf("SQLite did not retain the expected grant: %v", err)
			}
		})
	}
}

func TestOAuthStartWaitsForServerChanges(t *testing.T) {
	const owner = "owner"
	var discoveryRequests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		discoveryRequests.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(remote.Close)
	server := settings.MCPServer{Name: "fixture", URL: remote.URL + "/mcp", OAuth: &settings.MCPOAuthConfig{}}
	configured := true
	oauth := NewOAuth(nil, "https://shinona.example.test")
	handler := oauthHandler{oauth: oauth, logger: slog.New(slog.DiscardHandler),
		lookup: func(_ context.Context, account, name string) (settings.MCPServer, bool) {
			return server, configured && account == owner && name == server.Name
		},
	}
	start := func(ctx context.Context) *httptest.ResponseRecorder {
		request := httptest.NewRequestWithContext(auth.WithUser(ctx, auth.User{ID: owner}), http.MethodPost,
			"/auth/mcp/start", strings.NewReader(`{"name":"fixture"}`))
		response := httptest.NewRecorder()
		handler.start(response, request)
		return response
	}
	// While the settings change is pending, the former endpoint is still
	// visible in the repository. Starting OAuth must wait for the edit.
	if err := oauth.ApplyServerChanges(t.Context(), owner, func() ([]string, error) {
		ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
		defer cancel()
		response := start(ctx)
		if ctx.Err() != context.DeadlineExceeded || discoveryRequests.Load() != 0 || response.Header().Get("Set-Cookie") != "" {
			t.Errorf("start used a configuration being changed: status=%d, discovery requests=%d, context=%v",
				response.Code, discoveryRequests.Load(), ctx.Err())
		}
		configured = false
		return []string{server.Name}, nil
	}); err != nil {
		t.Fatal(err)
	}
	if response := start(t.Context()); response.Code != http.StatusNotFound || discoveryRequests.Load() != 0 {
		t.Fatalf("start did not see the committed removal: status=%d, discovery requests=%d", response.Code, discoveryRequests.Load())
	}
}

func TestDisconnectCancelsPendingOAuthRequests(t *testing.T) {
	store, err := sqlite.Open(t.Context(), filepath.Join(t.TempDir(), "shinona.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	const owner = "owner"
	if err := store.Accounts.CreatePasswordUser(t.Context(), auth.User{ID: owner, Email: "owner@example.test"}, []byte("test-hash")); err != nil {
		t.Fatal(err)
	}
	discovering := make(chan struct{})
	stopped := make(chan struct{})
	exchanging := make(chan struct{})
	exchangeStopped := make(chan struct{})
	var firstRequest atomic.Bool
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", func(w http.ResponseWriter, r *http.Request) {
		if firstRequest.CompareAndSwap(false, true) {
			close(discovering)
			<-r.Context().Done()
			close(stopped)
			return
		}
		w.WriteHeader(http.StatusUnauthorized)
	})
	mux.HandleFunc("/.well-known/oauth-protected-resource/mcp", func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		writeOAuthJSON(w, map[string]any{"resource": issuer + "/mcp", "authorization_servers": []string{issuer}})
	})
	mux.HandleFunc("/.well-known/oauth-authorization-server", func(w http.ResponseWriter, r *http.Request) {
		issuer := "http://" + r.Host
		writeOAuthJSON(w, map[string]any{
			"issuer": issuer, "authorization_endpoint": issuer + "/authorize", "token_endpoint": issuer + "/token",
			"code_challenge_methods_supported": []string{"S256"},
		})
	})
	mux.HandleFunc("POST /token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		close(exchanging)
		<-r.Context().Done()
		close(exchangeStopped)
	})
	remote := httptest.NewServer(mux)
	t.Cleanup(remote.Close)
	oauth := NewOAuth(store.Settings, "https://shinona.example.test")
	server := settings.MCPServer{Name: "fixture", URL: remote.URL + "/mcp", OAuth: &settings.MCPOAuthConfig{ClientID: "registered"}}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	handler := oauthHandler{oauth: oauth, logger: slog.New(slog.DiscardHandler),
		lookup: func(_ context.Context, account, name string) (settings.MCPServer, bool) {
			return server, account == owner && name == server.Name
		},
	}
	request := httptest.NewRequestWithContext(auth.WithUser(ctx, auth.User{ID: owner}), http.MethodPost,
		"/auth/mcp/start", strings.NewReader(`{"name":"fixture"}`))
	response := httptest.NewRecorder()
	finished := make(chan struct{})
	go func() {
		defer close(finished)
		handler.start(response, request)
	}()
	select {
	case <-discovering:
	case <-ctx.Done():
		t.Fatal("OAuth discovery did not start")
	}
	if err := oauth.ApplyServerChanges(ctx, "other", func() ([]string, error) { return []string{server.Name}, nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
		t.Fatal("another account canceled discovery")
	default:
	}
	if err := oauth.Disconnect(ctx, owner, server.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case <-finished:
		var body struct{ Code, URL string }
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil || response.Code != http.StatusConflict ||
			body.Code != "MCP_OAUTH_START_FAILED" || body.URL != "" || response.Header().Get("Set-Cookie") != "" {
			t.Fatalf("invalidated discovery returned HTTP %d with body %s", response.Code, response.Body)
		}
	case <-ctx.Done():
		t.Fatal("disconnect did not interrupt discovery")
	}
	select {
	case <-stopped:
	case <-ctx.Done():
		t.Fatal("discovery request remained open on the OAuth server")
	}
	state, destination, err := oauth.Start(ctx, owner, server.Name, "", handler.lookup)
	if err != nil || state == "" || destination == "" {
		t.Fatalf("explicit reconnect did not start a new flow: %v", err)
	}
	callback := make(chan error, 1)
	go func() {
		callback <- oauth.Finish(ctx, owner, state, "fixture-code", "", handler.lookup)
	}()
	select {
	case <-exchanging:
	case <-ctx.Done():
		t.Fatal("OAuth exchange did not start")
	}
	if err := oauth.Disconnect(ctx, owner, server.Name); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-callback:
		if !errors.Is(err, ErrOAuthFlow) {
			t.Fatalf("exchange was not invalidated: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("disconnect did not interrupt the callback")
	}
	select {
	case <-exchangeStopped:
	case <-ctx.Done():
		t.Fatal("token exchange remained open on the OAuth server")
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
