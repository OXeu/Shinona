package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	OAuthCallback     = "/auth/mcp/callback"
	OAuthMetadata     = "/auth/mcp/client.json"
	OAuthFlowLifetime = 10 * time.Minute
	maxOAuthFlows     = 256
	maxFlowsPerOwner  = 8
)

var (
	ErrOAuthRequired       = errors.New("connect this MCP server in Settings")
	ErrOAuthClientRequired = errors.New("this MCP server does not offer automatic OAuth client registration")
	ErrOAuthFlow           = errors.New("MCP OAuth sign-in expired or was cancelled")
)

type OAuthStore interface {
	MCPGrant(ctx context.Context, owner, server string) ([]byte, error)
	MCPGrants(ctx context.Context, owner string, servers []string) (map[string][]byte, error)
	SaveMCPGrant(ctx context.Context, owner, server string, data []byte) error
	ReplaceMCPGrant(ctx context.Context, owner, server string, previous, data []byte) (bool, error)
	DeleteMCPGrantIfUnchanged(ctx context.Context, owner, server string, previous []byte) (bool, error)
	DeleteMCPGrant(ctx context.Context, owner, server string) error
}

type OAuth struct {
	store          OAuthStore
	redirect       string
	clientMetadata string
	mu             sync.Mutex
	flows          map[string]*oauthFlow
	refreshing     keylock.Locks[oauthGrantKey]
	accountChanges keylock.Locks[string]
}

type oauthGrantKey struct{ owner, server string }

type oauthFlow struct {
	owner, name, verifier string
	expires               time.Time
	used                  bool
	grant                 oauthGrant
	cancelDiscovery       context.CancelCauseFunc
}

type oauthGrant struct {
	Fingerprint      [sha256.Size]byte
	Resource, Issuer string
	RequireIssuer    bool
	Config           oauth2.Config
	Token            *oauth2.Token
}

func NewOAuth(store OAuthStore, origin string) *OAuth {
	flow := &OAuth{store: store, redirect: origin + OAuthCallback, flows: make(map[string]*oauthFlow)}
	if strings.HasPrefix(origin, "https://") {
		flow.clientMetadata = origin + OAuthMetadata
	}
	return flow
}

func (o *OAuth) ClientMetadata() map[string]any {
	if o.clientMetadata == "" {
		return nil
	}
	return map[string]any{
		"client_id": o.clientMetadata, "client_name": "Shinona", "application_type": "web", "redirect_uris": []string{o.redirect},
		"grant_types": []string{"authorization_code", "refresh_token"}, "response_types": []string{"code"},
		"token_endpoint_auth_method": "none",
	}
}

func (o *OAuth) Start(ctx context.Context, owner string, server settings.MCPServer, previous string) (string, string, error) {
	if owner == "" || server.OAuth == nil {
		return "", "", ErrOAuthFlow
	}
	ctx, cancelDiscovery := context.WithCancelCause(ctx)
	defer cancelDiscovery(nil)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	state := oauth2.GenerateVerifier()
	flow := &oauthFlow{owner: owner, name: server.Name, verifier: oauth2.GenerateVerifier(), cancelDiscovery: cancelDiscovery}
	if !o.reserveFlow(state, previous, flow) {
		return "", "", errors.New("MCP OAuth sign-in is busy")
	}
	grant, err := discoverOAuth(ctx, server, o.redirect, o.clientMetadata)
	o.mu.Lock()
	defer o.mu.Unlock()
	if cause := context.Cause(ctx); cause != nil {
		err = cause
	}
	if err != nil {
		delete(o.flows, state)
		return "", "", err
	}
	grant.Fingerprint = fingerprint(server)
	flow.cancelDiscovery = nil
	flow.expires = time.Now().Add(OAuthFlowLifetime)
	flow.grant = grant
	return state, grant.Config.AuthCodeURL(state, oauth2.S256ChallengeOption(flow.verifier),
		oauth2.SetAuthURLParam("resource", grant.Resource)), nil
}

func (o *OAuth) reserveFlow(state, previous string, incoming *oauthFlow) bool {
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	owned := 0
	for key, flow := range o.flows {
		if flow.cancelDiscovery == nil && (!now.Before(flow.expires) || (key == previous && flow.owner == incoming.owner)) {
			delete(o.flows, key)
		} else if flow.owner == incoming.owner {
			owned++
		}
	}
	if owned >= maxFlowsPerOwner || len(o.flows) >= maxOAuthFlows {
		return false
	}
	o.flows[state] = incoming
	return true
}

func (o *OAuth) takeFlow(owner, state string) *oauthFlow {
	o.mu.Lock()
	defer o.mu.Unlock()
	flow := o.flows[state]
	if flow == nil || flow.owner != owner || flow.used || !time.Now().Before(flow.expires) {
		return nil
	}
	flow.used = true
	return flow
}

func (o *OAuth) Finish(ctx context.Context, owner, state, code, issuer string, lookup func(string) (settings.MCPServer, bool)) error {
	flow := o.takeFlow(owner, state)
	if flow == nil {
		return ErrOAuthFlow
	}
	defer func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.flows, state)
	}()
	grant := flow.grant
	server, ok := lookup(flow.name)
	if !ok || grant.Fingerprint != fingerprint(server) || code == "" ||
		((grant.RequireIssuer || issuer != "") && issuer != grant.Issuer) {
		return ErrOAuthFlow
	}
	exchange := timing.Start(ctx, "mcp_oauth_token_exchange")
	token, err := grant.Config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, oauthHTTP(server.URL)), code,
		oauth2.VerifierOption(flow.verifier), oauth2.SetAuthURLParam("resource", grant.Resource))
	exchange()
	if err != nil {
		return err
	}
	if !token.Valid() || !validBearer(token) {
		return errors.New("the MCP OAuth server did not return a usable bearer token")
	}
	grant.Token = token
	data, err := json.Marshal(grant)
	if err != nil {
		return err
	}
	release, err := o.accountChanges.Acquire(ctx, owner, false)
	if err != nil {
		return err
	}
	defer release()
	o.mu.Lock()
	valid := o.flows[state] == flow
	o.mu.Unlock()
	if current, ok := lookup(flow.name); !valid || !ok || fingerprint(current) != grant.Fingerprint {
		return ErrOAuthFlow
	}
	defer timing.Start(ctx, "mcp_oauth_save_grant")()
	return o.store.SaveMCPGrant(ctx, owner, flow.name, data)
}

func (o *OAuth) Disconnect(ctx context.Context, owner, name string) error {
	defer timing.Start(ctx, "mcp_oauth_disconnect")()
	return o.ApplyServerChanges(ctx, owner, func() ([]string, error) {
		return []string{name}, o.store.DeleteMCPGrant(ctx, owner, name)
	})
}

// ApplyServerChanges coordinates settings changes with in-flight callbacks.
// The commit saves the settings and deletes grants for the invalidated names.
func (o *OAuth) ApplyServerChanges(ctx context.Context, owner string, commit func() ([]string, error)) error {
	release, err := o.accountChanges.Acquire(ctx, owner, false)
	if err != nil {
		return err
	}
	defer release()
	names, err := commit()
	if err != nil || len(names) == 0 {
		return err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for state, flow := range o.flows {
		if flow.owner == owner && slices.Contains(names, flow.name) {
			if flow.cancelDiscovery != nil {
				// Keep the reservation until discovery returns so canceled
				// requests continue to count against the concurrency limit.
				flow.cancelDiscovery(ErrOAuthFlow)
			} else {
				delete(o.flows, state)
			}
		}
	}
	return nil
}

func (o *OAuth) grant(ctx context.Context, owner string, server settings.MCPServer) (oauthGrant, []byte, error) {
	data, err := o.store.MCPGrant(ctx, owner, server.Name)
	if err != nil {
		return oauthGrant{}, nil, err
	}
	grant, err := o.decodeGrant(server, data)
	return grant, data, err
}

func (o *OAuth) decodeGrant(server settings.MCPServer, data []byte) (oauthGrant, error) {
	var grant oauthGrant
	if len(data) == 0 || json.Unmarshal(data, &grant) != nil || grant.Token == nil ||
		grant.Fingerprint != fingerprint(server) || grant.Config.RedirectURL != o.redirect {
		return oauthGrant{}, ErrOAuthRequired
	}
	return grant, nil
}

func (o *OAuth) ConnectedServers(ctx context.Context, owner string, servers settings.MCPConfig) (map[string]bool, error) {
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		if server.OAuth != nil {
			names = append(names, server.Name)
		}
	}
	if len(names) == 0 {
		return nil, nil
	}
	grants, err := o.store.MCPGrants(ctx, owner, names)
	if err != nil {
		return nil, err
	}
	connected := make(map[string]bool, len(grants))
	for _, server := range servers {
		if grant, err := o.decodeGrant(server, grants[server.Name]); err == nil && grant.usable() {
			connected[server.Name] = true
		}
	}
	return connected, nil
}

func (g oauthGrant) usable() bool {
	return validBearer(g.Token) && (g.Token.Valid() || g.Token.RefreshToken != "")
}

func (o *OAuth) Token(ctx context.Context, owner string, server settings.MCPServer) (*oauth2.Token, error) {
	release, err := o.refreshing.Acquire(ctx, oauthGrantKey{owner, server.Name}, false)
	if err != nil {
		return nil, err
	}
	defer release()
	grant, previous, err := o.grant(ctx, owner, server)
	if err != nil {
		return nil, err
	}
	if !grant.usable() {
		return nil, ErrOAuthRequired
	}
	if grant.Token.Valid() {
		return grant.Token, nil
	}
	token, err := grant.refreshToken(ctx, oauthHTTP(server.URL))
	if err != nil {
		// Credentials and provider response bodies stay out of the chat.
		if failure, ok := errors.AsType[*oauth2.RetrieveError](err); ok && failure.ErrorCode == "invalid_grant" {
			deleted, writeErr := o.store.DeleteMCPGrantIfUnchanged(ctx, owner, server.Name, previous)
			if writeErr != nil {
				return nil, writeErr
			}
			if !deleted {
				return o.currentToken(ctx, owner, server)
			}
			return nil, ErrOAuthRequired
		}
		return nil, fmt.Errorf("could not refresh MCP OAuth for %s", server.Name)
	}
	if !validBearer(token) {
		return nil, ErrOAuthRequired
	}
	grant.Token = token
	data, err := json.Marshal(grant)
	if err != nil {
		return nil, err
	}
	saved, err := o.store.ReplaceMCPGrant(ctx, owner, server.Name, previous, data)
	if err != nil {
		return nil, err
	}
	if !saved {
		return o.currentToken(ctx, owner, server)
	}
	return token, nil
}

// A refresh can lose its write to a disconnect or a newer authorization.
func (o *OAuth) currentToken(ctx context.Context, owner string, server settings.MCPServer) (*oauth2.Token, error) {
	grant, _, err := o.grant(ctx, owner, server)
	if err != nil {
		return nil, err
	}
	if !grant.Token.Valid() || !validBearer(grant.Token) {
		return nil, ErrOAuthRequired
	}
	return grant.Token, nil
}

func fingerprint(server settings.MCPServer) [sha256.Size]byte {
	encoded, _ := json.Marshal(server)
	return sha256.Sum256(encoded)
}

func validBearer(token *oauth2.Token) bool {
	return token != nil && token.AccessToken != "" && strings.EqualFold(token.Type(), "Bearer") &&
		!strings.ContainsAny(token.AccessToken, "\r\n")
}

func (o *OAuth) transport(owner string, server settings.MCPServer, base http.RoundTripper) http.RoundTripper {
	return oauthTransport{oauth: o, owner: owner, server: server, base: base}
}

type oauthTransport struct {
	oauth  *OAuth
	owner  string
	server settings.MCPServer
	base   http.RoundTripper
}

func (t oauthTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.String() != t.server.URL {
		return nil, errors.New("MCP OAuth token can only be used at the configured endpoint")
	}
	token, err := t.oauth.Token(req.Context(), t.owner, t.server)
	if err != nil {
		return nil, err
	}
	return tokenTransport{base: t.base, token: token.AccessToken}.RoundTrip(req)
}
