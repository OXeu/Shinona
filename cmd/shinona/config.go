package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/joho/godotenv"

	"github.com/OXeu/shinona/internal/adapter/e2b"
	"github.com/OXeu/shinona/internal/adapter/email"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/adapter/oidc"
	"github.com/OXeu/shinona/internal/adapter/password"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/adapter/sqlite"
	"github.com/OXeu/shinona/internal/adapter/sshsandbox"
	"github.com/OXeu/shinona/internal/run"
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

func (e environment) boundedInt(key string, fallback, from, through int) (int, error) {
	raw := e.value(key, strconv.Itoa(fallback))
	value, err := strconv.Atoi(raw)
	if err != nil || value < from || value > through {
		return 0, fmt.Errorf("%s must be an integer between %d and %d", key, from, through)
	}
	return value, nil
}

func openDatabase(ctx context.Context, e environment) (*sqlite.Database, error) {
	config := sqlite.DefaultConfig()
	connections, err := e.boundedInt("SHINONA_SQLITE_READ_CONNECTIONS", config.ReadConnections, 1, sqlite.MaxReadConnections)
	if err != nil {
		return nil, err
	}
	config.ReadConnections = connections
	config.TranscriptCacheBytes, err = cacheBudget(e, "SHINONA_SQLITE_TRANSCRIPT_CACHE_MIB", config.TranscriptCacheBytes)
	if err != nil {
		return nil, err
	}
	return sqlite.OpenWithConfig(ctx, e.value("SHINONA_DATABASE", "shinona.db"), config)
}

func passwordHasher(e environment) (password.Argon2id, error) {
	defaults := password.DefaultParameters()
	timeCost, err := e.boundedInt("SHINONA_ARGON2_TIME", int(defaults.Time), 1, 10)
	if err != nil {
		return password.Argon2id{}, err
	}
	memory, err := e.boundedInt("SHINONA_ARGON2_MEMORY_KIB", int(defaults.MemoryKiB), 16*1024, 256*1024)
	if err != nil {
		return password.Argon2id{}, err
	}
	threads, err := e.boundedInt("SHINONA_ARGON2_THREADS", int(defaults.Threads), 1, 8)
	if err != nil {
		return password.Argon2id{}, err
	}
	return password.New(password.Parameters{Time: uint32(timeCost), MemoryKiB: uint32(memory), Threads: uint8(threads)})
}

func resendConfig(e environment) (email.ResendConfig, error) {
	config := email.DefaultResendConfig()
	config.Endpoint = e.value("SHINONA_RESEND_API_URL", config.Endpoint)
	endpoint, err := url.Parse(config.Endpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.User != nil || endpoint.ForceQuery || endpoint.RawQuery != "" ||
		strings.Contains(config.Endpoint, "#") || httpclient.CheckHost(endpoint.Hostname()) != nil {
		return email.ResendConfig{}, errors.New("SHINONA_RESEND_API_URL must be a public HTTPS URL without credentials, a query, or a fragment")
	}
	seconds, err := e.boundedInt("SHINONA_RESEND_TIMEOUT_SECONDS", int(config.Timeout/time.Second), 1, 60)
	if err != nil {
		return email.ResendConfig{}, err
	}
	config.Timeout = time.Duration(seconds) * time.Second
	return config, nil
}

func apiConfig(e environment) (rest.Config, error) {
	config := rest.DefaultConfig()
	limits := &config.Transcript
	var err error
	limits.Exports, err = e.boundedInt("SHINONA_TRANSCRIPT_EXPORT_CONCURRENCY", limits.Exports, 1, 32)
	if err != nil {
		return rest.Config{}, err
	}
	limits.Pages, err = e.boundedInt("SHINONA_TRANSCRIPT_PAGE_CONCURRENCY", limits.Pages, 1, 128)
	if err != nil {
		return rest.Config{}, err
	}
	limits.ResponseCacheBytes, err = cacheBudget(e, "SHINONA_TRANSCRIPT_RESPONSE_CACHE_MIB", limits.ResponseCacheBytes)
	if err != nil {
		return rest.Config{}, err
	}
	limits.TurnCacheBytes, err = cacheBudget(e, "SHINONA_TRANSCRIPT_TURN_CACHE_MIB", limits.TurnCacheBytes)
	if err != nil {
		return rest.Config{}, err
	}
	config.SessionListCacheBytes, err = cacheBudget(e, "SHINONA_SESSION_LIST_CACHE_MIB", config.SessionListCacheBytes)
	if err != nil {
		return rest.Config{}, err
	}
	return config, nil
}

func cacheBudget(e environment, key string, fallbackBytes int) (int, error) {
	const mib = 1 << 20
	value, err := e.boundedInt(key, fallbackBytes/mib, 0, 512)
	return value * mib, err
}

func trustedProxyNetworks(raw string) ([]netip.Prefix, error) {
	var networks []netip.Prefix
	for _, entry := range strings.Fields(strings.ReplaceAll(raw, ",", " ")) {
		prefix, err := netip.ParsePrefix(entry)
		if err != nil {
			return nil, errors.New("SHINONA_TRUSTED_PROXIES must list CIDR networks")
		}
		networks = append(networks, prefix.Masked())
	}
	return networks, nil
}

func e2bSandboxProvider(values settings.Values, defaults run.ProjectSandboxConfig, client *http.Client, store e2b.WorkspaceStore) (sandbox.Provider, error) {
	key := values[settings.E2BAPIKey]
	if key == "" {
		return nil, nil
	}
	boxes, err := e2b.New(e2b.Config{
		APIKey: key, Template: defaults.Template, BaseURL: values[settings.E2BBaseURL],
		ProxyURL: values[settings.E2BProxyURL], Domain: values[settings.E2BDomain],
		Timeout: time.Duration(defaults.TimeoutSeconds) * time.Second, OnTimeout: defaults.OnTimeout,
		APIVersion: values[settings.E2BAPIVersion], WorkspaceUser: values[settings.E2BWorkspaceUser],
		WorkDirectory: values[settings.E2BWorkDirectory],
	}, client, store)
	if err != nil {
		return nil, settings.Invalid("INVALID_E2B_CONFIG", err.Error())
	}
	return boxes, nil
}

func e2bSandboxDefaults(values settings.Values) (run.ProjectSandboxConfig, error) {
	config := run.ProjectSandboxConfig{Template: values[settings.E2BTemplate],
		TimeoutSeconds: settings.DefaultE2BTimeoutSeconds, OnTimeout: sandbox.TimeoutPause}
	if config.Template == "" {
		config.Template = settings.DefaultE2BTemplate
	}
	if value := values[settings.E2BTimeout]; value != "" {
		seconds, err := strconv.Atoi(value)
		if err != nil || seconds < 1 || seconds > sandbox.MaxTimeoutSeconds {
			return config, settings.Invalid("INVALID_E2B_TIMEOUT", "enter an E2B timeout between 1 and 86400 seconds")
		}
		config.TimeoutSeconds = seconds
	}
	if value := values[settings.E2BOnTimeout]; value != "" {
		if value != sandbox.TimeoutPause && value != sandbox.TimeoutKill {
			return config, settings.Invalid("INVALID_E2B_TIMEOUT", "the E2B timeout action must be pause or kill")
		}
		config.OnTimeout = value
	}
	return config, nil
}

func sshContainerConfig(e environment) (sshsandbox.ContainerConfig, error) {
	config := sshsandbox.DefaultContainerConfig()
	for _, option := range []struct {
		key   string
		value *int
		limit int
	}{
		{"SHINONA_SSH_CONTAINER_UID", &config.UID, 1<<31 - 1},
		{"SHINONA_SSH_CONTAINER_GID", &config.GID, 1<<31 - 1},
		{"SHINONA_SSH_CONTAINER_CPUS", &config.CPUs, 1024},
		{"SHINONA_SSH_CONTAINER_MEMORY_MIB", &config.MemoryMiB, 1 << 20},
		{"SHINONA_SSH_CONTAINER_PIDS", &config.Pids, 1 << 20},
	} {
		value, err := e.boundedInt(option.key, *option.value, 1, option.limit)
		if err != nil {
			return config, err
		}
		*option.value = value
	}
	config.WorkDirectory = e.value("SHINONA_SSH_CONTAINER_WORKDIR", config.WorkDirectory)
	return config, config.Validate()
}

func (r *sshRuntime) sshSandboxProvider(values settings.Values) (sandbox.Provider, error) {
	if values[settings.SSHAddress] == "" {
		return nil, nil
	}
	provider, err := sshsandbox.New(sshsandbox.Config{
		Address: values[settings.SSHAddress], User: values[settings.SSHUser],
		KeyPath: values[settings.SSHKeyPath], HostFingerprint: values[settings.SSHHostFingerprint],
		Template:  values[settings.SSHTemplate],
		Container: r.container, ProjectConnections: &r.projects,
	})
	if err != nil {
		return nil, settings.Invalid("INVALID_SSH_CONFIG", err.Error())
	}
	return provider, nil
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
		endpoint.Scheme != "https" {
		return nil, settings.Invalid("INVALID_OAUTH_ISSUER", "OAuth issuer must use HTTPS")
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
