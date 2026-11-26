// Package e2b provisions project sandboxes on E2B Cloud or self-hosted E2B.
package e2b

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"sync/atomic"
	"time"

	sdk "github.com/eric642/e2b-go-sdk"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	envdPort    = 49983
	awakeWindow = 5 * time.Minute
	apiTimeout  = 20 * time.Second
)

type Config struct {
	APIKey         string
	Template       string
	BaseURL        string
	ProxyURL       string
	Domain         string
	Timeout        time.Duration
	OnTimeout      string
	APIVersion     string
	WorkspaceUser  string
	WorkDirectory  string
	CleanupTimeout time.Duration
}

type Provider struct {
	api         *sdk.Client
	config      Config
	dataScheme  string
	sdkProxyURL string
	endpoint    string
	store       WorkspaceStore
	state       *providerState
}

// Project-specific provider copies share endpoint discovery and provisioning locks.
type providerState struct {
	apiVersion atomic.Int32
	provision  keylock.Locks[sandbox.ProjectKey]
}

// WorkspaceStore persists sandbox IDs and private data-plane credentials.
type WorkspaceStore interface {
	E2BSandbox(ctx context.Context, ownerID, projectID, endpoint string) (sandboxID string, credentials []byte, err error)
	// SaveE2BSandbox only writes if the current ID equals previousID, or if
	// previousID is empty and the project has no workspace.
	SaveE2BSandbox(ctx context.Context, ownerID, projectID, endpoint, previousID, sandboxID string, credentials []byte) (bool, error)
	UpdateE2BCredentials(ctx context.Context, ownerID, projectID, endpoint, sandboxID string, credentials []byte) error
	DeleteE2BSandbox(ctx context.Context, ownerID, projectID, endpoint, sandboxID string) error
}

type credentials struct {
	EnvdToken        string
	TrafficToken     string
	RecoverySnapshot string        `json:",omitempty"`
	WorkspaceUser    string        `json:",omitempty"`
	WorkDirectory    string        `json:",omitempty"`
	Timeout          time.Duration `json:",omitempty"`
}

type projectSandbox struct {
	id     string
	tokens credentials
}

func New(config Config, data *http.Client, store WorkspaceStore) (*Provider, error) {
	if strings.EqualFold(os.Getenv("E2B_DEBUG"), "true") {
		return nil, errors.New("E2B_DEBUG must be unset for sandbox provisioning")
	}
	if config.APIKey == "" {
		return nil, errors.New("E2B requires an API key")
	}
	if store == nil {
		return nil, errors.New("E2B requires a project workspace store")
	}
	switch config.APIVersion {
	case "", "auto", "v1", "v2":
	default:
		return nil, errors.New("E2B API version must be auto, v1, or v2")
	}
	if !validWorkspaceUser(config.WorkspaceUser) {
		return nil, errors.New("E2B workspace user must be a user name")
	}
	if config.WorkDirectory != "" && (!path.IsAbs(config.WorkDirectory) || strings.ContainsRune(config.WorkDirectory, 0)) {
		return nil, errors.New("E2B work directory must be an absolute path")
	}
	if config.Timeout == 0 {
		config.Timeout = awakeWindow
	}
	if config.OnTimeout == "" {
		config.OnTimeout = sandbox.TimeoutPause
	}
	if config.CleanupTimeout < 0 {
		return nil, errors.New("E2B cleanup timeout must be positive")
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = sandbox.DefaultCleanupTimeout
	}
	scheme := "https"
	if config.BaseURL != "" {
		endpoint, err := parseEndpoint(config.BaseURL)
		if err != nil {
			return nil, err
		}
		scheme = endpoint.Scheme
		if config.Domain == "" {
			config.Domain = strings.TrimPrefix(endpoint.Hostname(), "api.")
		}
	}
	if config.ProxyURL != "" {
		if _, err := parseEndpoint(config.ProxyURL); err != nil {
			return nil, err
		}
	}
	if config.Domain == "" {
		config.Domain = sdk.DefaultDomain
	}
	apiURL := config.BaseURL
	if apiURL == "" {
		apiURL = "https://api." + config.Domain
		parsed, err := parseEndpoint(apiURL)
		if err != nil || parsed.Host != "api."+config.Domain {
			return nil, errors.New("E2B domain must be a hostname")
		}
	}
	sdkConfig := (sdk.Config{
		APIKey: config.APIKey, APIURL: apiURL, SandboxURL: config.ProxyURL,
		Domain: config.Domain, RequestTimeout: apiTimeout, HTTPClient: sdkHTTPClient(data),
	}).Resolve()
	api, err := sdk.NewClient(sdkConfig)
	if err != nil {
		return nil, err
	}
	return &Provider{
		api: api, config: config, dataScheme: scheme, sdkProxyURL: sdkConfig.SandboxURL,
		endpoint: strings.TrimRight(apiURL, "/"), store: store, state: &providerState{},
	}, nil
}

func validWorkspaceUser(user string) bool {
	return len(user) <= 255 && !strings.ContainsAny(user, ":/ \t\r\n\x00")
}

func parseEndpoint(value string) (*url.URL, error) {
	endpoint, err := url.Parse(value)
	if err != nil || endpoint.Hostname() == "" || endpoint.User != nil || endpoint.RawQuery != "" || endpoint.ForceQuery ||
		endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("E2B endpoints must be HTTP(S) base URLs without credentials, query, or fragment")
	}
	return endpoint, nil
}

func (*Provider) Policies(string) []sandbox.Policy { return []sandbox.Policy{sandbox.Shared} }

func (p *Provider) WithIdleTimeout(policy sandbox.IdleTimeout) sandbox.ManagedProjectProvider {
	configured := *p
	if policy.After > 0 {
		configured.config.Timeout = policy.After
	}
	if policy.Action != "" {
		configured.config.OnTimeout = policy.Action
	}
	return &configured
}

func (p *Provider) Acquire(ctx context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	if spec.Policy != sandbox.Shared {
		return nil, sandbox.ErrPolicy
	}
	release, err := p.state.provision.Acquire(ctx, spec.ProjectKey(), false)
	if err != nil {
		return nil, err
	}
	defer release()
	workspace, err := p.acquireProject(ctx, spec)
	if err != nil {
		return nil, err
	}
	workspace.keepAwake(ctx, p.config.Timeout)
	return workspace, nil
}

func (p *Provider) acquireProject(ctx context.Context, spec sandbox.Spec) (*projectWorkspace, error) {
	previous, err := p.loadProject(ctx, spec)
	if err != nil {
		return nil, err
	}
	if previous.id == "" {
		return p.createProject(ctx, spec, projectSandbox{}, "")
	}
	workspace, err := p.reconnectProject(ctx, spec, previous)
	if !errors.Is(err, sandbox.ErrWorkspaceMissing) {
		return workspace, err
	}
	if p.config.OnTimeout == sandbox.TimeoutKill {
		return p.createProject(ctx, spec, previous, "")
	}
	return p.recoverProject(ctx, spec, previous)
}

func (p *Provider) OpenProject(ctx context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	project, err := p.loadProject(ctx, spec)
	if err != nil {
		return nil, err
	}
	workspace, err := p.reconnectProject(ctx, spec, project)
	if err != nil {
		return nil, err
	}
	return workspace, nil
}

func (p *Provider) loadProject(ctx context.Context, spec sandbox.Spec) (projectSandbox, error) {
	id, stored, err := p.store.E2BSandbox(ctx, spec.OwnerID, spec.ProjectID, p.endpoint)
	if err != nil {
		return projectSandbox{}, err
	}
	return decodeProject(id, stored)
}

func decodeProject(id string, stored []byte) (projectSandbox, error) {
	project := projectSandbox{id: id}
	if id != "" {
		if err := json.Unmarshal(stored, &project.tokens); err != nil {
			return projectSandbox{}, fmt.Errorf("read E2B sandbox credentials: %w", err)
		}
	}
	return project, nil
}

func (p *Provider) reconnectProject(ctx context.Context, spec sandbox.Spec, project projectSandbox) (*projectWorkspace, error) {
	id := project.id
	if id == "" {
		return nil, sandbox.ErrWorkspaceMissing
	}
	options := sdk.ConnectOptions{Timeout: p.config.Timeout}
	box, err := p.requestSandbox(ctx, func(ctx context.Context, legacy bool) (*sdk.Sandbox, error) {
		if legacy {
			return p.api.ConnectV1(ctx, id, options)
		}
		return p.api.Connect(ctx, id, options)
	})
	if err != nil {
		if isGone(err) {
			return nil, fmt.Errorf("%w: %w", sandbox.ErrWorkspaceMissing, err)
		}
		return nil, fmt.Errorf("reconnect E2B project sandbox %s: %w", id, err)
	}
	if box.ID != id {
		return nil, fmt.Errorf("E2B returned an unexpected sandbox while reconnecting %s", id)
	}
	// Connect can extend the expiration. Overwrite it when the previous
	// duration was longer or the workspace predates saved timeout settings.
	if project.tokens.Timeout == 0 || project.tokens.Timeout > p.config.Timeout {
		if err := box.SetTimeout(ctx, p.config.Timeout); err != nil {
			return nil, fmt.Errorf("set E2B project sandbox timeout: %w", err)
		}
	}
	tokens := boxCredentials(box, project.tokens)
	tokens.Timeout = p.config.Timeout
	workspace, tokens, err := p.dataPlane(ctx, box, tokens, true)
	if err != nil {
		return nil, err
	}
	if tokens != project.tokens {
		encoded, err := json.Marshal(tokens)
		if err != nil {
			return nil, err
		}
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := p.store.UpdateE2BCredentials(save, spec.OwnerID, spec.ProjectID, p.endpoint, id, encoded); err != nil {
			return nil, err
		}
	}
	return workspace, nil
}

func (p *Provider) createProject(ctx context.Context, spec sandbox.Spec, previous projectSandbox, snapshot string) (*projectWorkspace, error) {
	options := sdk.CreateOptions{
		Template:               p.config.Template,
		Timeout:                p.config.Timeout,
		IdempotencyKey:         spec.RunID,
		Lifecycle:              &sdk.LifecycleOptions{OnTimeout: p.config.OnTimeout},
		AllowInternetAccess:    true,
		AllowInternetAccessSet: true,
		// Some self-hosted runtimes issue the traffic token only for private ingress.
		Network: &sdk.NetworkOptions{AllowPublicTraffic: new(false)},
	}
	if spec.Template != "" {
		options.Template = spec.Template
	}
	if snapshot != "" {
		options.Template = snapshot
		options.IdempotencyKey = snapshotKey(spec.RunID, snapshot)
	}
	box, err := p.requestSandbox(ctx, func(ctx context.Context, legacy bool) (*sdk.Sandbox, error) {
		if legacy {
			return p.api.CreateV1(ctx, options)
		}
		return p.api.Create(ctx, options)
	})
	if err != nil {
		if snapshot != "" && isGone(err) {
			return nil, fmt.Errorf("%w: %w", errSnapshotMissing, err)
		}
		return nil, err
	}

	tokens := credentials{RecoverySnapshot: snapshot, Timeout: p.config.Timeout}
	if snapshot != "" {
		tokens.WorkspaceUser, tokens.WorkDirectory = previous.tokens.WorkspaceUser, previous.tokens.WorkDirectory
	}
	workspace, tokens, err := p.dataPlane(ctx, box, boxCredentials(box, tokens), snapshot != "")
	if err != nil {
		return p.reconcileCreatedProject(ctx, spec, box, err)
	}
	saved, err := p.saveProject(ctx, spec, previous.id, box.ID, tokens)
	if err != nil {
		return p.reconcileCreatedProject(ctx, spec, box, err)
	}
	if saved {
		return workspace, nil
	}
	return p.reconcileCreatedProject(ctx, spec, box, nil)
}

// An explicitly selected version never probes another API. Auto starts with
// v2 and remembers the last working version for this endpoint.
func (p *Provider) requestSandbox(ctx context.Context, request func(context.Context, bool) (*sdk.Sandbox, error)) (*sdk.Sandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	switch p.config.APIVersion {
	case "v1":
		return request(ctx, true)
	case "v2":
		return request(ctx, false)
	}
	first := p.state.apiVersion.Load() == 1
	box, err := request(ctx, first)
	if err == nil || !routeUnavailable(err) {
		return box, err
	}
	box, fallback := request(ctx, !first)
	if fallback == nil {
		if first {
			p.state.apiVersion.Store(2)
		} else {
			p.state.apiVersion.Store(1)
		}
		return box, nil
	}
	if !routeUnavailable(fallback) || (isGone(fallback) && !isGone(err)) {
		return nil, fallback
	}
	return nil, err
}

func routeUnavailable(err error) bool {
	return hasStatus(err, http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented)
}

func (p *Provider) reconcileCreatedProject(ctx context.Context, spec sandbox.Spec, box *sdk.Sandbox, failure error) (*projectWorkspace, error) {
	// A canceled request can still have created a sandbox. Resolve ownership
	// before cleanup because an idempotent response might already be retained.
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.config.CleanupTimeout)
	defer cancel()
	id, stored, err := p.store.E2BSandbox(cleanup, spec.OwnerID, spec.ProjectID, p.endpoint)
	if err != nil {
		return nil, errors.Join(failure, fmt.Errorf("check retained project sandbox before cleanup: %w", err))
	}
	if id != box.ID {
		if err := box.Kill(cleanup); err != nil && !isGone(err) {
			return nil, errors.Join(failure, fmt.Errorf("discard unused E2B sandbox: %w", err))
		}
	}
	if failure != nil || ctx.Err() != nil {
		return nil, errors.Join(failure, ctx.Err())
	}
	current, err := decodeProject(id, stored)
	if err != nil {
		return nil, err
	}
	return p.reconnectProject(ctx, spec, current)
}

func (p *Provider) saveProject(ctx context.Context, spec sandbox.Spec, previousID, id string, tokens credentials) (bool, error) {
	encoded, err := json.Marshal(tokens)
	if err != nil {
		return false, err
	}
	// A canceled turn can still have provisioned a workspace for the project.
	save, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.store.SaveE2BSandbox(save, spec.OwnerID, spec.ProjectID, p.endpoint, previousID, id, encoded)
}

func boxCredentials(box *sdk.Sandbox, previous credentials) credentials {
	// Some self-hosted connect responses omit the still-valid traffic token.
	if token := box.CurrentEnvdAccessToken(); token != "" {
		previous.EnvdToken = token
	}
	if token := box.CurrentTrafficAccessToken(); token != "" {
		previous.TrafficToken = token
	}
	return previous
}

var _ sandbox.ManagedProjectProvider = (*Provider)(nil)
var _ sandbox.IdleTimeoutProvider = (*Provider)(nil)
