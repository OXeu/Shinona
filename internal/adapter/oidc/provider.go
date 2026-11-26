// Package oidc implements OAuth 2.0 login with an OpenID Connect provider.
package oidc

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"net/url"
	"sync/atomic"
	"time"

	core "github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/oauth2"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/timing"
)

type Provider struct {
	issuer   string
	metadata *core.Provider
	oauth    oauth2.Config
	verifier atomic.Pointer[core.IDTokenVerifier]
	client   *http.Client
	validate func(string) error
}

func New(ctx context.Context, issuer, clientID, secret, redirect string) (*Provider, error) {
	client := httpclient.Public()
	client.Timeout = 10 * time.Second
	return newProvider(ctx, issuer, clientID, secret, redirect, client, checkEndpoint)
}

// NewWithClient allows an explicit transport for deployments with a separate
// network trust policy, or for tests with a local identity provider.
func NewWithClient(ctx context.Context, issuer, clientID, secret, redirect string, client *http.Client) (*Provider, error) {
	return newProvider(ctx, issuer, clientID, secret, redirect, client, func(string) error { return nil })
}

func newProvider(ctx context.Context, issuer, clientID, secret, redirect string, client *http.Client, validate func(string) error) (*Provider, error) {
	if err := validate(issuer); err != nil {
		return nil, err
	}
	provider, err := discover(ctx, issuer, client, validate)
	if err != nil {
		return nil, err
	}
	p := &Provider{
		issuer:   issuer,
		metadata: provider,
		oauth: oauth2.Config{
			ClientID: clientID, ClientSecret: secret, RedirectURL: redirect,
			Endpoint: provider.Endpoint(), Scopes: []string{core.ScopeOpenID, "email", "profile"},
		},
		client: client, validate: validate,
	}
	if err := p.cacheVerifier(provider); err != nil {
		return nil, err
	}
	return p, nil
}

func checkEndpoint(raw string) error {
	if err := httpclient.CheckEndpoint(raw); err != nil {
		return err
	}
	target, err := url.Parse(raw)
	if err != nil || target.Scheme != "https" || target.Fragment != "" {
		return errors.New("OIDC endpoints must use HTTPS")
	}
	return nil
}

func discover(ctx context.Context, issuer string, client *http.Client, validate func(string) error) (*core.Provider, error) {
	provider, err := core.NewProvider(core.ClientContext(ctx, client), issuer)
	if err != nil {
		return nil, err
	}
	var metadata struct {
		JWKSURI  string `json:"jwks_uri"`
		UserInfo string `json:"userinfo_endpoint"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return nil, err
	}
	endpoint := provider.Endpoint()
	for _, raw := range []string{endpoint.AuthURL, endpoint.TokenURL} {
		if err := validate(raw); err != nil {
			return nil, err
		}
	}
	for _, raw := range []string{metadata.JWKSURI, metadata.UserInfo} {
		if raw != "" {
			if err := validate(raw); err != nil {
				return nil, err
			}
		}
	}
	return provider, nil
}

func (p *Provider) AuthorizationURL(state string, flow auth.Flow) string {
	return p.oauth.AuthCodeURL(state, core.Nonce(flow.Nonce), oauth2.S256ChallengeOption(flow.Verifier))
}

func (p *Provider) Exchange(ctx context.Context, code string, flow auth.Flow) (auth.Identity, error) {
	ctx = core.ClientContext(ctx, p.client)
	exchange := timing.Start(ctx, "oidc_token_exchange")
	token, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(flow.Verifier))
	exchange()
	if err != nil {
		return auth.Identity{}, err
	}
	raw, ok := token.Extra("id_token").(string)
	if !ok {
		return auth.Identity{}, errors.New("OAuth response is missing the ID token")
	}
	verify := timing.Start(ctx, "oidc_verify_token")
	verified, err := p.verifyToken(ctx, raw)
	verify()
	if err != nil {
		return auth.Identity{}, err
	}
	if subtle.ConstantTimeCompare([]byte(verified.Nonce), []byte(flow.Nonce)) != 1 {
		return auth.Identity{}, errors.New("OAuth ID token nonce did not match")
	}
	var claims struct {
		Email           string `json:"email"`
		EmailVerified   bool   `json:"email_verified"`
		AuthorizedParty string `json:"azp"`
	}
	if err := verified.Claims(&claims); err != nil {
		return auth.Identity{}, err
	}
	if (len(verified.Audience) > 1 || claims.AuthorizedParty != "") && claims.AuthorizedParty != p.oauth.ClientID {
		return auth.Identity{}, errors.New("OAuth ID token was issued to a different client")
	}
	identity := auth.Identity{Issuer: verified.Issuer, Subject: verified.Subject}
	if claims.EmailVerified {
		identity.Email = claims.Email
	}
	identity.Name, identity.AvatarURL = profileClaims(verified)
	if identity.AvatarURL == "" && token.AccessToken != "" {
		userinfo := timing.Start(ctx, "oidc_userinfo")
		info, err := p.metadata.UserInfo(ctx, oauth2.StaticTokenSource(token))
		userinfo()
		if err == nil && info.Subject == verified.Subject {
			name, avatarURL := profileClaims(info)
			if identity.Name == "" {
				identity.Name = name
			}
			if identity.AvatarURL == "" {
				identity.AvatarURL = avatarURL
			}
		}
	}
	return identity, nil
}

func profileClaims(claims interface{ Claims(any) error }) (string, string) {
	var profile struct {
		Name    string `json:"name"`
		Picture string `json:"picture"`
	}
	if err := claims.Claims(&profile); err != nil {
		return "", ""
	}
	image, err := url.Parse(profile.Picture)
	if err != nil || image.Hostname() == "" || image.User != nil || (image.Scheme != "https" && image.Scheme != "http") {
		return profile.Name, ""
	}
	return profile.Name, image.String()
}

func (p *Provider) verifyToken(ctx context.Context, raw string) (*core.IDToken, error) {
	verifier := p.verifier.Load()
	if verifier == nil {
		// An issuer can publish its signing keys after the initial discovery.
		provider, err := discover(ctx, p.issuer, p.client, p.validate)
		if err != nil {
			return nil, err
		}
		if err := p.cacheVerifier(provider); err != nil {
			return nil, err
		}
		verifier = p.verifier.Load()
		if verifier == nil {
			return nil, errors.New("OIDC discovery is missing jwks_uri")
		}
	}
	return verifier.Verify(ctx, raw)
}

func (p *Provider) cacheVerifier(provider *core.Provider) error {
	var metadata struct {
		JWKSURI string `json:"jwks_uri"`
	}
	if err := provider.Claims(&metadata); err != nil {
		return err
	}
	if metadata.JWKSURI != "" {
		p.verifier.CompareAndSwap(nil, provider.Verifier(&core.Config{ClientID: p.oauth.ClientID}))
	}
	return nil
}
