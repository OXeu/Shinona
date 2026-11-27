package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/sync/singleflight"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	OAuthCallback          = "/auth/mcp/callback"
	OAuthMetadata          = "/auth/mcp/client.json"
	OAuthFlowLifetime      = 10 * time.Minute
	maxOAuthFlows          = 256
	maxFlowsPerOwner       = 8
	oauthRefreshTimeout    = 12 * time.Second
	oauthFinishTimeout     = 12 * time.Second
	oauthGrantWriteTimeout = 5 * time.Second
)

var (
	ErrOAuthRequired       = errors.New("connect this MCP server in Settings")
	ErrOAuthClientRequired = errors.New("this MCP server does not offer automatic OAuth client registration")
	ErrOAuthFlow           = errors.New("MCP OAuth sign-in expired or was cancelled")
	errOAuthServerNotFound = errors.New("MCP OAuth server not found")
)

type OAuthStore interface {
	MCPGrant(ctx context.Context, owner, server string) ([]byte, error)
	MCPGrants(ctx context.Context, owner string, servers []string) (map[string][]byte, error)
	SaveMCPGrant(ctx context.Context, owner, server string, data []byte) error
	ReplaceMCPGrant(ctx context.Context, owner, server string, previous, data []byte) (bool, error)
	DeleteMCPGrantIfUnchanged(ctx context.Context, owner, server string, previous []byte) (bool, error)
	DeleteMCPGrant(ctx context.Context, owner, server string) error
}

type oauthServerLookup func(context.Context, string, string) (settings.MCPServer, bool)

type OAuth struct {
	store          OAuthStore
	redirect       string
	clientMetadata string
	mu             sync.Mutex
	flows          map[string]*oauthFlow
	refreshing     singleflight.Group
	accountChanges keylock.Locks[string]
}

type oauthFlow struct {
	owner, name, verifier string
	expires               time.Time
	grant                 oauthGrant
	cancelPending         context.CancelCauseFunc
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

func (o *OAuth) Start(ctx context.Context, owner, name, previous string, lookup oauthServerLookup) (string, string, error) {
	if owner == "" {
		return "", "", ErrOAuthFlow
	}
	ctx, cancelPending := context.WithCancelCause(ctx)
	defer cancelPending(nil)
	ctx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	state := oauth2.GenerateVerifier()
	flow := &oauthFlow{owner: owner, name: name, verifier: oauth2.GenerateVerifier(), cancelPending: cancelPending}
	server, err := o.reserveFlow(ctx, state, previous, flow, lookup)
	if err != nil {
		return "", "", err
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
	flow.cancelPending = nil
	flow.expires = time.Now().Add(OAuthFlowLifetime)
	flow.grant = grant
	return state, grant.Config.AuthCodeURL(state, oauth2.S256ChallengeOption(flow.verifier),
		oauth2.SetAuthURLParam("resource", grant.Resource)), nil
}

func (o *OAuth) reserveFlow(ctx context.Context, state, previous string, incoming *oauthFlow, lookup oauthServerLookup) (settings.MCPServer, error) {
	// A settings edit must finish before the lookup, or see this reservation
	// when it invalidates existing flows. Discovery runs after releasing it.
	release, err := o.accountChanges.Acquire(ctx, incoming.owner, true)
	if err != nil {
		return settings.MCPServer{}, err
	}
	defer release()
	server, ok := lookup(ctx, incoming.owner, incoming.name)
	if err := ctx.Err(); err != nil {
		return settings.MCPServer{}, err
	}
	if !ok || server.OAuth == nil {
		return settings.MCPServer{}, errOAuthServerNotFound
	}
	now := time.Now()
	o.mu.Lock()
	defer o.mu.Unlock()
	owned := 0
	for key, flow := range o.flows {
		replaced := key == previous && flow.owner == incoming.owner
		if flow.cancelPending == nil && (!now.Before(flow.expires) || replaced) {
			delete(o.flows, key)
			continue
		}
		if replaced {
			flow.cancelPending(ErrOAuthFlow)
		}
		if flow.owner == incoming.owner {
			owned++
		}
	}
	if owned >= maxFlowsPerOwner || len(o.flows) >= maxOAuthFlows {
		return settings.MCPServer{}, errors.New("MCP OAuth sign-in is busy")
	}
	o.flows[state] = incoming
	return server, nil
}

func (o *OAuth) takeFlow(owner, state string, cancelPending context.CancelCauseFunc) *oauthFlow {
	o.mu.Lock()
	defer o.mu.Unlock()
	flow := o.flows[state]
	if flow == nil || flow.owner != owner || flow.cancelPending != nil || !time.Now().Before(flow.expires) {
		return nil
	}
	flow.cancelPending = cancelPending
	return flow
}

func (o *OAuth) Finish(ctx context.Context, owner, state, code, issuer string, lookup oauthServerLookup) error {
	ctx, cancel := context.WithTimeout(ctx, oauthFinishTimeout)
	defer cancel()
	ctx, cancelPending := context.WithCancelCause(ctx)
	defer cancelPending(nil)
	flow := o.takeFlow(owner, state, cancelPending)
	if flow == nil {
		return ErrOAuthFlow
	}
	defer func() {
		o.mu.Lock()
		defer o.mu.Unlock()
		delete(o.flows, state)
	}()
	grant := flow.grant
	server, ok := lookup(ctx, owner, flow.name)
	if !ok || grant.Fingerprint != fingerprint(server) || code == "" ||
		((grant.RequireIssuer || issuer != "") && issuer != grant.Issuer) {
		return ErrOAuthFlow
	}
	exchange := timing.Start(ctx, "mcp_oauth_token_exchange")
	token, err := grant.Config.Exchange(context.WithValue(ctx, oauth2.HTTPClient, oauthHTTP(server.URL)), code,
		oauth2.VerifierOption(flow.verifier), oauth2.SetAuthURLParam("resource", grant.Resource))
	exchange()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
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
		if cause := context.Cause(ctx); cause != nil {
			return cause
		}
		return err
	}
	defer release()
	if cause := context.Cause(ctx); cause != nil {
		return cause
	}
	if current, ok := lookup(ctx, owner, flow.name); !ok || fingerprint(current) != grant.Fingerprint {
		return ErrOAuthFlow
	}
	defer timing.Start(ctx, "mcp_oauth_save_grant")()
	return o.store.SaveMCPGrant(ctx, owner, flow.name, data)
}

func (o *OAuth) Disconnect(ctx context.Context, owner, name string) error {
	return o.disconnect(ctx, owner, name, nil)
}

func (o *OAuth) disconnect(ctx context.Context, owner, name string, lookup oauthServerLookup) error {
	defer timing.Start(ctx, "mcp_oauth_disconnect")()
	return o.ApplyServerChanges(ctx, owner, func() ([]string, error) {
		if lookup != nil {
			if server, ok := lookup(ctx, owner, name); !ok || server.OAuth == nil {
				return nil, errOAuthServerNotFound
			}
		}
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
			if flow.cancelPending != nil {
				// Keep the reservation until the request returns so canceled
				// requests continue to count against the concurrency limit.
				flow.cancelPending(ErrOAuthFlow)
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
	grant, _, err := o.grant(ctx, owner, server)
	if err != nil {
		return nil, err
	}
	if !grant.usable() {
		return nil, ErrOAuthRequired
	}
	if grant.Token.Valid() {
		return grant.Token, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	config := fingerprint(server)
	result := o.refreshing.DoChan(owner+"\x00"+string(config[:]), func() (any, error) {
		// Once an issuer rotates the refresh token, save the replacement
		// even if the caller has stopped waiting for it.
		refreshCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthRefreshTimeout)
		defer cancel()
		return o.refresh(refreshCtx, owner, server)
	})
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case refreshed := <-result:
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if refreshed.Err != nil {
			return nil, refreshed.Err
		}
		return refreshed.Val.(*oauth2.Token), nil
	}
}

func (o *OAuth) refresh(ctx context.Context, owner string, server settings.MCPServer) (*oauth2.Token, error) {
	// Another refresh may have saved a new token since the caller read the
	// expired grant. Check again before contacting the issuer.
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
		if failure, ok := errors.AsType[*oauth2.RetrieveError](err); ok {
			slog.WarnContext(ctx, "MCP OAuth refresh rejected", "server", server.Name, "status", failure.Response.StatusCode)
			if failure.ErrorCode == "invalid_grant" {
				save, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthGrantWriteTimeout)
				defer cancel()
				deleted, writeErr := o.store.DeleteMCPGrantIfUnchanged(save, owner, server.Name, previous)
				if writeErr != nil {
					return nil, writeErr
				}
				if !deleted {
					return o.currentToken(save, owner, server)
				}
				return nil, ErrOAuthRequired
			}
		} else {
			if requestErr, ok := errors.AsType[*url.Error](err); ok {
				err = requestErr.Err
			}
			slog.WarnContext(ctx, "MCP OAuth refresh failed", "server", server.Name, "error", err)
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
	// The issuer may already have invalidated the old refresh token. Give
	// the conditional write its own deadline after the exchange completes.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), oauthGrantWriteTimeout)
	defer cancel()
	saved, err := o.store.ReplaceMCPGrant(save, owner, server.Name, previous, data)
	if err != nil {
		return nil, err
	}
	if !saved {
		return o.currentToken(save, owner, server)
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
		!strings.ContainsAny(token.AccessToken, "\x00\r\n")
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
