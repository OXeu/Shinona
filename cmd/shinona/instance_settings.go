package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/mail"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/OXeu/shinona/internal/adapter/e2b"
	"github.com/OXeu/shinona/internal/adapter/email"
	"github.com/OXeu/shinona/internal/adapter/httpauth"
	"github.com/OXeu/shinona/internal/adapter/rest"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

// instanceSettings builds the live service configuration from a committed revision.
type instanceSettings struct {
	repo       settings.Repository
	defaults   settings.Values
	origin     *url.URL
	publicHTTP *http.Client
	e2bHTTP    *http.Client
	e2bStore   e2b.WorkspaceStore
	buildSSH   func(settings.Values) (sandbox.Provider, error)
	agent      func(context.Context, string) (run.Agent, error)
	current    atomic.Pointer[runtimeConfig]
}

type runtimeConfig struct {
	revision  int64
	overrides settings.Values
	values    settings.Values
	auth      httpauth.Config
	e2b       run.Target
	ssh       run.Target
}

var _ rest.InstanceSettings = (*instanceSettings)(nil)

func (r *instanceSettings) initialize(ctx context.Context) error {
	for {
		stored, err := r.repo.SettingOverrides(ctx)
		if err != nil {
			return err
		}
		overrides, err := settings.Apply(stored.Values, nil)
		if err != nil {
			return err
		}
		next, err := r.prepare(ctx, overrides, nil)
		if err != nil {
			return err
		}
		next.revision = stored.Revision
		if len(overrides) != len(stored.Values) {
			saved, err := r.repo.SaveSettingOverrides(ctx, stored.Revision, overrides)
			if err != nil {
				return err
			}
			if !saved {
				continue
			}
			next.revision++
		}
		r.current.Store(next)
		return nil
	}
}

func (r *instanceSettings) prepare(ctx context.Context, overrides settings.Values, previous *runtimeConfig) (*runtimeConfig, error) {
	values := settings.Effective(r.defaults, overrides)
	var mailer auth.VerificationMailer
	from, mailKey := values[settings.MailFrom], values[settings.MailAPIKey]
	if from != "" {
		if address, err := mail.ParseAddress(from); err != nil || !strings.Contains(address.Address, "@") || strings.ContainsAny(from, "\r\n") {
			return nil, settings.Invalid("INVALID_MAIL_FROM", "enter a valid sender email address")
		}
		if mailKey != "" {
			mailer = email.Resend{APIKey: mailKey, From: from, Client: r.publicHTTP}
		}
	}
	e2bDefaults, err := e2bSandboxDefaults(values)
	if err != nil {
		return nil, err
	}
	e2bTarget := run.Target{Name: sandbox.ProviderE2B, Defaults: e2bDefaults, ResolveAgent: r.agent}
	sshTarget := run.Target{Name: sandbox.ProviderSSH, ResolveAgent: r.agent,
		Defaults: run.ProjectSandboxConfig{Template: values[settings.SSHTemplate]}}
	if previous != nil && sameSettings(previous.values, values, settings.E2BAPIKey, settings.E2BTemplate,
		settings.E2BBaseURL, settings.E2BProxyURL, settings.E2BDomain, settings.E2BTimeout, settings.E2BOnTimeout,
		settings.E2BAPIVersion, settings.E2BWorkspaceUser, settings.E2BWorkDirectory) {
		e2bTarget.Provider = previous.e2b.Provider
	} else {
		e2bTarget.Provider, err = e2bSandboxProvider(values, e2bDefaults, r.e2bHTTP, r.e2bStore)
		if err != nil {
			return nil, err
		}
	}
	if previous != nil && sameSettings(previous.values, values, settings.SSHAddress, settings.SSHUser,
		settings.SSHKeyPath, settings.SSHHostFingerprint, settings.SSHTemplate) {
		sshTarget.Provider = previous.ssh.Provider
	} else {
		sshTarget.Provider, err = r.buildSSH(values)
		if err != nil {
			return nil, err
		}
	}
	var oauth httpauth.OAuthProvider
	if previous != nil && sameSettings(previous.values, values, settings.OAuthIssuer, settings.OAuthClientID, settings.OAuthSecret) {
		oauth = previous.auth.OAuth
	} else {
		oauth, err = oauthProvider(ctx, values, r.origin)
		if err != nil {
			return nil, err
		}
	}
	fingerprint := sha256.Sum256([]byte(strings.Join([]string{
		r.origin.String(), values[settings.OAuthIssuer], values[settings.OAuthClientID],
	}, "\x00")))
	authConfig := httpauth.Config{OAuth: oauth, Mailer: mailer, Secure: r.origin.Scheme == "https",
		FlowVersion: hex.EncodeToString(fingerprint[:])}
	return &runtimeConfig{overrides: overrides, values: values, e2b: e2bTarget, ssh: sshTarget, auth: authConfig}, nil
}

func sameSettings(a, b settings.Values, keys ...settings.Key) bool {
	for _, key := range keys {
		if a[key] != b[key] {
			return false
		}
	}
	return true
}

func (r *instanceSettings) Resolve(name string) run.Target {
	config := r.current.Load()
	switch name {
	case "":
		if config.e2b.Provider == nil && config.ssh.Provider != nil {
			return config.ssh
		}
		return config.e2b
	case sandbox.ProviderE2B:
		return config.e2b
	case sandbox.ProviderSSH:
		return config.ssh
	default:
		return run.Target{Name: name}
	}
}

func (r *instanceSettings) Mode() string { return r.Resolve("").Name }

func (r *instanceSettings) Host() string { return r.origin.Host }

func (r *instanceSettings) Auth() httpauth.Config { return r.current.Load().auth }

func (r *instanceSettings) Fields() []settings.Field {
	config := r.current.Load()
	return settings.Fields(config.values, config.overrides)
}

func (r *instanceSettings) Update(ctx context.Context, changes []settings.Change) ([]settings.Field, error) {
	current := r.current.Load()
	for {
		overrides, err := settings.Apply(current.overrides, changes)
		if err != nil {
			return nil, err
		}
		next, err := r.prepare(ctx, overrides, current)
		if err != nil {
			return nil, err
		}
		saved, err := r.repo.SaveSettingOverrides(ctx, current.revision, next.overrides)
		if err != nil {
			return nil, err
		}
		if saved {
			next.revision = current.revision + 1
			r.publish(next)
			return settings.Fields(next.values, next.overrides), nil
		}
		current, err = r.reload(ctx, current)
		if err != nil {
			return nil, err
		}
	}
}

func (r *instanceSettings) reload(ctx context.Context, previous *runtimeConfig) (*runtimeConfig, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if current := r.current.Load(); current.revision > previous.revision {
		return current, nil
	}
	// A writer may have committed before publishing, or committed through
	// another repository connection. Rebuild from the durable snapshot.
	stored, err := r.repo.SettingOverrides(ctx)
	if err != nil {
		return nil, err
	}
	if current := r.current.Load(); current.revision >= stored.Revision {
		return current, nil
	}
	current, err := r.prepare(ctx, stored.Values, previous)
	if err != nil {
		return nil, err
	}
	current.revision = stored.Revision
	return r.publish(current), nil
}

func (r *instanceSettings) publish(next *runtimeConfig) *runtimeConfig {
	for {
		current := r.current.Load()
		if current.revision >= next.revision {
			return current
		}
		if r.current.CompareAndSwap(current, next) {
			return next
		}
	}
}
