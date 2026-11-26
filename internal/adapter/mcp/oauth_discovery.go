package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"strings"
	"time"

	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
	"golang.org/x/oauth2"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/settings"
	"github.com/OXeu/shinona/internal/timing"
)

func discoverOAuth(ctx context.Context, server settings.MCPServer, redirect, metadataURL string) (oauthGrant, error) {
	client := oauthHTTP(server.URL)
	resourceURL, err := checkedOAuthURL(server.URL, isLoopbackURL(server.URL))
	if err != nil {
		return oauthGrant{}, err
	}
	resourceLookup := timing.Start(ctx, "mcp_oauth_resource")
	resource, scopes, err := protectedResource(ctx, client, resourceURL)
	resourceLookup()
	if err != nil {
		return oauthGrant{}, err
	}
	issuer := resource.AuthorizationServers[0]
	local := isLoopbackURL(server.URL)
	if _, err := checkedOAuthURL(issuer, local); err != nil {
		return oauthGrant{}, err
	}
	issuerLookup := timing.Start(ctx, "mcp_oauth_issuer")
	metadata, err := sdkauth.GetAuthServerMetadata(ctx, issuer, client)
	issuerLookup()
	if err != nil {
		return oauthGrant{}, fmt.Errorf("read authorization server metadata: %w", err)
	}
	if metadata == nil || !slices.Contains(metadata.CodeChallengeMethodsSupported, "S256") {
		return oauthGrant{}, errors.New("the authorization server must publish metadata and support PKCE S256")
	}
	for _, endpoint := range []string{metadata.AuthorizationEndpoint, metadata.TokenEndpoint} {
		if _, err := checkedOAuthURL(endpoint, local); err != nil {
			return oauthGrant{}, err
		}
	}
	clientID, secret, style, err := oauthClient(ctx, client, metadata, server.OAuth, redirect, metadataURL, local)
	if err != nil {
		return oauthGrant{}, err
	}
	if slices.Contains(metadata.ScopesSupported, "offline_access") && !slices.Contains(scopes, "offline_access") {
		scopes = append(scopes, "offline_access")
	}
	return oauthGrant{
		Resource: resource.Resource,
		Issuer:   metadata.Issuer, RequireIssuer: metadata.AuthorizationResponseIssParameterSupported,
		Config: oauth2.Config{ClientID: clientID, ClientSecret: secret, RedirectURL: redirect, Scopes: scopes,
			Endpoint: oauth2.Endpoint{AuthURL: metadata.AuthorizationEndpoint, TokenURL: metadata.TokenEndpoint, AuthStyle: style}},
	}, nil
}

func oauthClient(ctx context.Context, client *http.Client, metadata *oauthex.AuthServerMeta, configured *settings.MCPOAuthConfig, redirect, metadataURL string, local bool) (string, string, oauth2.AuthStyle, error) {
	if configured != nil && configured.ClientID != "" {
		methods := metadata.TokenEndpointAuthMethodsSupported
		switch {
		case configured.ClientSecret == "" && (len(methods) == 0 || slices.Contains(methods, "none")):
			return configured.ClientID, "", oauth2.AuthStyleInParams, nil
		case configured.ClientSecret != "" && (len(methods) == 0 || slices.Contains(methods, "client_secret_basic")):
			return configured.ClientID, configured.ClientSecret, oauth2.AuthStyleInHeader, nil
		case configured.ClientSecret != "" && slices.Contains(methods, "client_secret_post"):
			return configured.ClientID, configured.ClientSecret, oauth2.AuthStyleInParams, nil
		default:
			return "", "", 0, errors.New("the configured MCP OAuth client cannot use an advertised token authentication method")
		}
	}
	if metadataURL != "" && metadata.ClientIDMetadataDocumentSupported {
		return metadataURL, "", oauth2.AuthStyleInParams, nil
	}
	if metadata.RegistrationEndpoint == "" {
		return "", "", 0, ErrOAuthClientRequired
	}
	if _, err := checkedOAuthURL(metadata.RegistrationEndpoint, local); err != nil {
		return "", "", 0, err
	}
	grantTypes := []string{"authorization_code"}
	if len(metadata.GrantTypesSupported) == 0 || slices.Contains(metadata.GrantTypesSupported, "refresh_token") {
		grantTypes = append(grantTypes, "refresh_token")
	}
	applicationType := "web"
	if isLoopbackURL(redirect) {
		applicationType = "native"
	}
	register := timing.Start(ctx, "mcp_oauth_register")
	registration, err := oauthex.RegisterClient(ctx, metadata.RegistrationEndpoint, &oauthex.ClientRegistrationMetadata{
		ClientName: "Shinona", RedirectURIs: []string{redirect}, TokenEndpointAuthMethod: "none",
		GrantTypes: grantTypes, ResponseTypes: []string{"code"}, ApplicationType: applicationType,
	}, client)
	register()
	if err != nil {
		return "", "", 0, errors.New("dynamic MCP OAuth client registration failed")
	}
	if registration.ClientID == "" {
		return "", "", 0, errors.New("dynamic MCP OAuth registration returned no client ID")
	}
	switch registration.TokenEndpointAuthMethod {
	case "", "none":
		return registration.ClientID, "", oauth2.AuthStyleInParams, nil
	case "client_secret_basic", "client_secret_post":
		if registration.ClientSecret != "" {
			style := oauth2.AuthStyleInParams
			if registration.TokenEndpointAuthMethod == "client_secret_basic" {
				style = oauth2.AuthStyleInHeader
			}
			return registration.ClientID, registration.ClientSecret, style, nil
		}
	}
	return "", "", 0, errors.New("dynamic MCP OAuth registration returned an unusable client authentication method")
}

func protectedResource(ctx context.Context, client *http.Client, resource *url.URL) (*oauthex.ProtectedResourceMetadata, []string, error) {
	path := *resource
	path.Path, path.RawPath = "/.well-known/oauth-protected-resource"+strings.TrimSuffix(resource.Path, "/"), ""
	base := *resource
	base.Path, base.RawPath = "", ""
	root := base.JoinPath(".well-known/oauth-protected-resource")
	type location struct{ metadata, audience string }
	locations := []location{{path.String(), resource.String()}, {root.String(), resource.String()}, {root.String(), base.String()}}
	var scopes []string
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, resource.String(), nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	if response, err := client.Do(req); err == nil {
		_ = response.Body.Close()
		if response.StatusCode == http.StatusUnauthorized {
			challenges, _ := oauthex.ParseWWWAuthenticate(response.Header.Values("WWW-Authenticate"))
			for _, challenge := range challenges {
				if challenge.Scheme == "bearer" {
					scopes = strings.Fields(challenge.Params["scope"])
					if metadata := challenge.Params["resource_metadata"]; metadata != "" {
						locations = append([]location{{metadata, resource.String()}}, locations...)
					}
					break
				}
			}
		}
	}
	for _, location := range locations {
		if _, err := checkedOAuthURL(location.metadata, isLoopbackURL(resource.String())); err != nil {
			continue
		}
		metadata, err := oauthex.GetProtectedResourceMetadata(ctx, location.metadata, location.audience, client)
		if err != nil || metadata == nil || len(metadata.AuthorizationServers) == 0 {
			continue
		}
		if len(scopes) == 0 {
			scopes = metadata.ScopesSupported
		}
		return metadata, scopes, nil
	}
	return nil, nil, errors.New("the MCP server did not publish usable protected resource metadata")
}

func isLoopbackURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && loopbackHost(u.Hostname())
}

func loopbackHost(host string) bool {
	ip, err := netip.ParseAddr(host)
	return strings.EqualFold(host, "localhost") || err == nil && ip.Unmap().IsLoopback()
}

func checkedOAuthURL(raw string, local bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, errors.New("invalid MCP OAuth URL")
	}
	loopback := loopbackHost(u.Hostname())
	localHTTP := u.Scheme == "http" && local && loopback
	if (u.Scheme != "https" && !localHTTP) || (!local && loopback) {
		return nil, errors.New("MCP OAuth requires HTTPS or a loopback endpoint for local development")
	}
	return u, nil
}

func oauthHTTP(resource string) *http.Client {
	transport := httpclient.PublicTransport(5*time.Second, isLoopbackURL(resource))
	transport.DisableKeepAlives = true
	return &http.Client{Transport: transport, Timeout: 7 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}
