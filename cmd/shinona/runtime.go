package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OXeu/shinona/internal/adapter/e2b"
	"github.com/OXeu/shinona/internal/adapter/email"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/adapter/mcp"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

type runtimeRepository interface {
	settings.Repository
	settings.ModelRepository
	settings.MCPRepository
	settings.SSHRepository
	e2b.WorkspaceStore
	mcp.OAuthStore
}

// runtime publishes immutable configurations after validation and persistence.
type runtime struct {
	repo        runtimeRepository
	defaults    settings.Values
	origin      *url.URL
	client      *http.Client
	mcpOAuth    *mcp.OAuth
	models      *modelRuntime
	accountHTTP *http.Client
	mcpMu       sync.Mutex
	sshMu       sync.Mutex
	trustedSSH  map[string]bool
	mu          sync.Mutex
	current     atomic.Pointer[runtimeConfig]
}

type runtimeConfig struct {
	overrides settings.Values
	values    settings.Values
	oauth     httpauth.OAuthProvider
	mailer    auth.VerificationMailer
	e2b       run.Target
	ssh       run.Target
}

func newRuntime(ctx context.Context, repo runtimeRepository, environment environment) (*runtime, error) {
	origin, err := webOrigin(environment.value("SHINONA_WEB_ORIGIN", "http://127.0.0.1:5173"))
	if err != nil {
		return nil, err
	}
	accountHTTP := httpclient.Public()
	modelHTTP, err := httpclient.ForModels(environment.value("SHINONA_TRUSTED_MODEL_ORIGINS", ""))
	if err != nil {
		return nil, fmt.Errorf("SHINONA_TRUSTED_MODEL_ORIGINS: %w", err)
	}
	trustedSSH, err := trustedSSHAddresses(environment.value("SHINONA_TRUSTED_SSH_ADDRESSES", ""))
	if err != nil {
		return nil, err
	}
	config := &runtime{
		repo: repo, defaults: settings.Defaults(environment.lookup), origin: origin,
		mcpOAuth:    mcp.NewOAuth(repo, origin.String()),
		models:      newModelRuntime(repo, modelHTTP),
		accountHTTP: accountHTTP,
		trustedSSH:  trustedSSH,
		client: &http.Client{
			Timeout:       35 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	stored, err := repo.SettingOverrides(ctx)
	if err != nil {
		return nil, err
	}
	overrides, err := settings.Apply(stored, nil)
	if err != nil {
		return nil, err
	}
	next, err := config.prepare(ctx, overrides, nil)
	if err != nil {
		return nil, err
	}
	if len(overrides) != len(stored) {
		if err := repo.SaveSettingOverrides(ctx, overrides); err != nil {
			return nil, err
		}
	}
	config.current.Store(next)
	return config, nil
}

func (r *runtime) prepare(ctx context.Context, overrides settings.Values, previous *runtimeConfig) (*runtimeConfig, error) {
	values := settings.Effective(r.defaults, overrides)
	var mailer auth.VerificationMailer
	from, mailKey := values[settings.MailFrom], values[settings.MailAPIKey]
	if from != "" {
		if address, err := mail.ParseAddress(from); err != nil || !strings.Contains(address.Address, "@") || strings.ContainsAny(from, "\r\n") {
			return nil, settings.Invalid("INVALID_MAIL_FROM", "enter a valid sender email address")
		}
		if mailKey != "" {
			mailer = email.Resend{APIKey: mailKey, From: from, Client: r.accountHTTP}
		}
	}
	e2bDefaults, err := e2bSandboxDefaults(values)
	if err != nil {
		return nil, err
	}
	e2bTarget := run.Target{Name: "e2b", Defaults: e2bDefaults, ResolveAgent: r.resolveAgent}
	sshTarget := run.Target{Name: "ssh", ResolveAgent: r.resolveAgent,
		Defaults: run.ProjectSandboxConfig{Template: values[settings.SSHTemplate]}}
	if previous != nil && sameSettings(previous.values, values, settings.E2BAPIKey, settings.E2BTemplate,
		settings.E2BBaseURL, settings.E2BProxyURL, settings.E2BDomain, settings.E2BTimeout, settings.E2BOnTimeout) {
		e2bTarget.Provider = previous.e2b.Provider
	} else {
		e2bTarget.Provider, err = e2bSandboxProvider(values, e2bDefaults, r.client, r.repo)
		if err != nil {
			return nil, err
		}
	}
	if previous != nil && sameSettings(previous.values, values, settings.SSHAddress, settings.SSHUser,
		settings.SSHKeyPath, settings.SSHHostFingerprint, settings.SSHTemplate) {
		sshTarget.Provider = previous.ssh.Provider
	} else {
		sshTarget.Provider, err = sshSandboxProvider(values)
		if err != nil {
			return nil, err
		}
	}
	var oauth httpauth.OAuthProvider
	if previous != nil && sameSettings(previous.values, values, settings.OAuthIssuer, settings.OAuthClientID, settings.OAuthSecret) {
		oauth = previous.oauth
	} else {
		oauth, err = oauthProvider(ctx, values, r.origin)
		if err != nil {
			return nil, err
		}
	}
	return &runtimeConfig{overrides: overrides, values: values,
		e2b: e2bTarget, ssh: sshTarget, oauth: oauth, mailer: mailer}, nil
}

func sameSettings(a, b settings.Values, keys ...settings.Key) bool {
	for _, key := range keys {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}

func (r *runtime) Resolve(name string) run.Target {
	config := r.current.Load()
	switch name {
	case "":
		if config.e2b.Provider == nil && config.ssh.Provider != nil {
			return config.ssh
		}
		return config.e2b
	case "e2b":
		return config.e2b
	case "ssh":
		return config.ssh
	default:
		return run.Target{Name: name}
	}
}

func (r *runtime) Mode() string { return r.Resolve("").Name }

func (r *runtime) Host() string { return r.origin.Host }

func (r *runtime) Auth() httpauth.Config {
	config := r.current.Load()
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{
		r.origin.String(), config.values[settings.OAuthIssuer], config.values[settings.OAuthClientID],
	}, "\x00")))
	return httpauth.Config{OAuth: config.oauth, Mailer: config.mailer, Secure: r.origin.Scheme == "https",
		FlowVersion: hex.EncodeToString(fingerprint[:])}
}

func (r *runtime) Fields() []settings.Field {
	config := r.current.Load()
	return settings.Fields(config.values, config.overrides)
}

func (r *runtime) Update(ctx context.Context, changes []settings.Change) ([]settings.Field, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	current := r.current.Load()
	overrides, err := settings.Apply(current.overrides, changes)
	if err != nil {
		return nil, err
	}
	next, err := r.publish(ctx, overrides, current)
	if err != nil {
		return nil, err
	}
	return settings.Fields(next.values, next.overrides), nil
}

// publish validates and persists a configuration before making it visible to readers.
func (r *runtime) publish(ctx context.Context, overrides settings.Values, current *runtimeConfig) (*runtimeConfig, error) {
	next, err := r.prepare(ctx, overrides, current)
	if err != nil {
		return nil, err
	}
	if err := r.repo.SaveSettingOverrides(ctx, overrides); err != nil {
		return nil, err
	}
	r.current.Store(next)
	return next, nil
}
