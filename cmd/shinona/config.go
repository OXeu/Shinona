package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/OXeu/shinona/internal/adapter/e2b"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/oidc"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

type environment map[string]string

func readEnvironment(path string) (environment, error) {
	values, err := godotenv.Read(path)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, errors.New("could not read .env")
	}
	return values, nil
}

func (e environment) lookup(key string) (string, bool) {
	if value := os.Getenv(key); value != "" {
		return value, true
	}
	value := e[key]
	return value, value != ""
}

func (e environment) value(key, fallback string) string {
	if value, ok := e.lookup(key); ok {
		return value
	}
	return fallback
}

func sandboxProvider(values settings.Values, client *http.Client, store e2b.WorkspaceStore) (sandbox.Provider, error) {
	key := values[settings.E2BAPIKey]
	if key == "" {
		return nil, nil
	}
	boxes, err := e2b.New(e2b.Config{
		APIKey: key, Template: values[settings.E2BTemplate], BaseURL: values[settings.E2BBaseURL],
		ProxyURL: values[settings.E2BProxyURL], Domain: values[settings.E2BDomain],
	}, client, store)
	if err != nil {
		return nil, settings.Invalid("INVALID_E2B_CONFIG", err.Error())
	}
	return boxes, nil
}

func webOrigin(value string) (*url.URL, error) {
	const message = "SHINONA_WEB_ORIGIN must be an HTTPS origin or an HTTP origin on loopback or a private IP address"
	origin, err := url.Parse(value)
	if err != nil || origin.Hostname() == "" || origin.User != nil || origin.Path != "" ||
		origin.RawQuery != "" || origin.Fragment != "" || origin.ForceQuery {
		return nil, errors.New(message)
	}
	host := origin.Hostname()
	local := isLoopback(host) || net.ParseIP(host).IsPrivate()
	if origin.Scheme != "https" && (origin.Scheme != "http" || !local) {
		return nil, errors.New(message)
	}
	return origin, nil
}

func oauthProvider(ctx context.Context, values settings.Values, origin *url.URL) (httpauth.OAuthProvider, error) {
	issuer, id, secret := values[settings.OAuthIssuer], values[settings.OAuthClientID], values[settings.OAuthSecret]
	if issuer == "" && id == "" && secret == "" {
		return nil, nil
	}
	if issuer == "" || id == "" || secret == "" {
		return nil, settings.Invalid("MISSING_OAUTH_CONFIG", "OAuth requires an issuer, client ID, and client secret")
	}
	endpoint, err := url.Parse(issuer)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.ForceQuery || endpoint.Fragment != "" ||
		(endpoint.Scheme != "https" && (endpoint.Scheme != "http" || !isLoopback(endpoint.Hostname()))) {
		return nil, settings.Invalid("INVALID_OAUTH_ISSUER", "OAuth issuer must be an HTTPS issuer or an HTTP loopback issuer")
	}
	discovery, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	provider, err := oidc.New(discovery, issuer, id, secret, origin.String()+"/auth/oauth/callback")
	if err != nil {
		return nil, settings.Invalid("OAUTH_DISCOVERY_FAILED", "could not discover the OAuth issuer")
	}
	return provider, nil
}

func listenAddress(environment environment) (string, error) {
	address := environment.value("SHINONA_ADDR", "127.0.0.1:8780")
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return "", fmt.Errorf("SHINONA_ADDR: %w", err)
	}
	if !isLoopback(host) {
		return "", errors.New("SHINONA_ADDR must use a loopback host")
	}
	return address, nil
}

func isLoopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || ip.IsLoopback()
}
