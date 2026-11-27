// Package sshsandbox manages Docker-backed project workspaces on an SSH host.
package sshsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	connectTimeout   = 10 * time.Second
	provisionTimeout = 2 * time.Minute
)

var (
	ErrInvalidFingerprint = errors.New("SSH requires a SHA256 host key fingerprint")
	ErrInvalidPrivateKey  = errors.New("SSH requires a valid private key of at most 64 KiB")
	ErrEncryptedKey       = errors.New("the SSH private key is protected by a passphrase")
	ErrHostKeyMismatch    = errors.New("the SSH host key fingerprint does not match")
	ErrAuthentication     = errors.New("the SSH server did not accept public key authentication")
	ErrHandshake          = errors.New("the SSH handshake failed")
	ErrDockerUnavailable  = errors.New("the SSH user cannot access Docker")
)

type Config struct {
	Address         string
	User            string
	KeyPath         string
	HostFingerprint string
	Template        string
	PrivateKey      string
	DialContext     func(context.Context, string, string) (net.Conn, error)
	Container       ContainerConfig
	CleanupTimeout  time.Duration
	// Providers sharing ProjectConnections must use the same dial policy.
	ProjectConnections *ProjectConnections
}

type Provider struct {
	address      string
	client       *ssh.ClientConfig
	template     string
	dial         func(context.Context, string, string) (net.Conn, error)
	projects     *ProjectConnections
	identity     connectionIdentity
	container    ContainerConfig
	cleanup      time.Duration
	legacyLimits *responsecache.Cache[string, struct{}]
}

var provisionGates keylock.Locks[sandbox.ProjectKey]

func projectHash(spec sandbox.Spec) [sha256.Size]byte {
	return sha256.Sum256([]byte(spec.OwnerID + "\x00" + spec.ProjectID))
}

func New(config Config) (*Provider, error) {
	host, port, err := net.SplitHostPort(config.Address)
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || host == "" || number < 1 || number > 65535 {
		return nil, errors.New("SSH address must be a host and port")
	}
	if config.User == "" || config.Template == "" {
		return nil, errors.New("SSH requires a user and default Docker image")
	}
	container := config.Container
	if container == (ContainerConfig{}) {
		container = DefaultContainerConfig()
	}
	if err := container.Validate(); err != nil {
		return nil, err
	}
	if config.CleanupTimeout < 0 {
		return nil, errors.New("SSH cleanup timeout must be positive")
	}
	if config.CleanupTimeout == 0 {
		config.CleanupTimeout = sandbox.DefaultCleanupTimeout
	}
	fingerprint := config.HostFingerprint
	decoded, err := base64.RawStdEncoding.DecodeString(strings.TrimPrefix(fingerprint, "SHA256:"))
	if err != nil || len(decoded) != sha256.Size || !strings.HasPrefix(fingerprint, "SHA256:") {
		return nil, ErrInvalidFingerprint
	}
	key, err := privateKey(config)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		if _, encrypted := errors.AsType[*ssh.PassphraseMissingError](err); encrypted {
			return nil, ErrEncryptedKey
		}
		return nil, ErrInvalidPrivateKey
	}
	client := &ssh.ClientConfig{
		User: config.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != fingerprint {
				return ErrHostKeyMismatch
			}
			return nil
		},
	}
	dial := config.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	projects := config.ProjectConnections
	if projects == nil {
		projects = &ProjectConnections{}
	}
	identity := connectionIdentity{
		address: config.Address, user: config.User, hostFingerprint: fingerprint,
		keyFingerprint: ssh.FingerprintSHA256(signer.PublicKey()),
	}
	return &Provider{
		address: config.Address, client: client, template: config.Template, dial: dial,
		projects: projects, identity: identity, container: container, cleanup: config.CleanupTimeout,
		legacyLimits: responsecache.New[string, struct{}](256, 16<<10),
	}, nil
}

func privateKey(config Config) ([]byte, error) {
	if config.KeyPath == "" {
		if config.PrivateKey == "" || len(config.PrivateKey) > sandbox.MaxFileBytes {
			return nil, ErrInvalidPrivateKey
		}
		return []byte(config.PrivateKey), nil
	}
	info, err := os.Stat(config.KeyPath)
	if err != nil || !info.Mode().IsRegular() || info.Size() > sandbox.MaxFileBytes {
		return nil, errors.New("SSH private key path must point to a readable key file")
	}
	file, err := os.Open(config.KeyPath)
	if err != nil {
		return nil, errors.New("SSH private key path must point to a readable key file")
	}
	defer func() { _ = file.Close() }()
	info, err = file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > sandbox.MaxFileBytes {
		return nil, errors.New("SSH private key path must point to a readable key file")
	}
	key, err := io.ReadAll(io.LimitReader(file, sandbox.MaxFileBytes+1))
	if err != nil {
		return nil, errors.New("could not read the SSH private key file")
	}
	if len(key) > sandbox.MaxFileBytes {
		return nil, ErrInvalidPrivateKey
	}
	return key, nil
}

func (p *Provider) connect(ctx context.Context, spec sandbox.Spec) (*workspace, error) {
	client, err := p.connectClient(ctx)
	if err != nil {
		return nil, err
	}
	return p.newWorkspace(client, spec), nil
}

func (p *Provider) connectClient(ctx context.Context) (*ssh.Client, error) {
	defer timing.Start(ctx, "ssh_connect")()
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	dialed := timing.Start(ctx, "ssh_dial")
	conn, err := p.dial(ctx, "tcp", p.address)
	dialed()
	if err != nil {
		return nil, fmt.Errorf("dial the sandbox SSH host: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	handshake := timing.Start(ctx, "ssh_handshake")
	client, channels, requests, err := ssh.NewClientConn(conn, p.address, p.client)
	handshake()
	if !stop() || err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: %w", ErrHandshake, err)
	}
	return ssh.NewClient(client, channels, requests), nil
}

func (p *Provider) newWorkspace(client *ssh.Client, spec sandbox.Spec) *workspace {
	return &workspace{client: client, name: fmt.Sprintf("shinona-%x", projectHash(spec)), cleanup: p.cleanup}
}

func (*Provider) Policies(string) []sandbox.Policy { return []sandbox.Policy{sandbox.Shared} }

// TestConnection checks SSH authentication and Docker access without changing the host.
func (p *Provider) TestConnection(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	probe := *p
	client := *p.client
	client.AuthCallback = func(attempt *ssh.ClientAuthContext) (ssh.AuthMethod, error) {
		if len(attempt.AllowedMethods) > 0 && (!slices.Contains(attempt.AllowedMethods, "publickey") ||
			slices.Contains(attempt.TriedMethods, "publickey")) {
			return nil, ErrAuthentication
		}
		return nil, nil
	}
	probe.client = &client
	ws, err := probe.connect(ctx, sandbox.Spec{})
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close(ctx) }()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close(ctx) })
	defer stop()
	if err := ws.docker(ctx, "info", "--format", "{{.ServerVersion}}\n"); err != nil {
		return errors.Join(ErrDockerUnavailable, ctx.Err(), err)
	}
	return nil
}

func (p *Provider) Acquire(ctx context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	if spec.Policy != sandbox.Shared {
		return nil, sandbox.ErrPolicy
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, provisionTimeout)
		defer cancel()
	}
	release, err := provisionGates.Acquire(ctx, spec.ProjectKey(), false)
	if err != nil {
		return nil, err
	}
	defer release()
	ws, err := p.openProject(ctx, spec, func(ws *workspace) error {
		stop := context.AfterFunc(ctx, ws.abortTransport)
		defer stop()
		return p.ensureRunning(ctx, ws, spec)
	})
	if err != nil {
		return nil, err
	}
	ws.shared = true
	return ws, nil
}

func (p *Provider) OpenProject(ctx context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	ws, err := p.openProject(ctx, spec, func(ws *workspace) error { return ws.requireRunningProject(ctx) })
	if err != nil {
		return nil, err
	}
	return ws, nil
}

func (p *Provider) InspectProject(ctx context.Context, spec sandbox.Spec, inspect func(context.Context, sandbox.Workspace) error) error {
	ws, err := p.openProject(ctx, spec, func(ws *workspace) error {
		// A successful inspection already proved the container could run the
		// command. On failure, distinguish a missing or stopped container from
		// an ordinary workspace error, such as a path that does not exist.
		err := inspect(ctx, ws)
		if err == nil || ws.discard.Load() || ctx.Err() != nil {
			return err
		}
		if statusErr := ws.requireRunningProject(ctx); statusErr != nil {
			return statusErr
		}
		return err
	})
	if err != nil {
		return err
	}
	defer timing.Start(ctx, "workspace_release")()
	// Keep an acknowledged inspection successful if lease cleanup fails.
	_ = ws.Close(ctx)
	return nil
}

func (p *Provider) openProject(ctx context.Context, spec sandbox.Spec, prepare func(*workspace) error) (*workspace, error) {
	key := projectConnectionKey{owner: spec.OwnerID, identity: p.identity}
	ws, retry, err := p.tryProject(ctx, spec, key, p.projects.take(key), prepare)
	if retry {
		// A transport can fail after Docker acted, including on its first use.
		// Provisioning checks the same container again before making changes.
		ws, _, err = p.tryProject(ctx, spec, key, nil, prepare)
	}
	return ws, err
}

func (p *Provider) tryProject(ctx context.Context, spec sandbox.Spec, key projectConnectionKey, conn *projectConnection, prepare func(*workspace) error) (lease *workspace, retry bool, err error) {
	if conn == nil {
		client, err := p.connectClient(ctx)
		if err != nil {
			return nil, false, err
		}
		conn = &projectConnection{client: client, opened: time.Now()}
	}
	ws := p.newWorkspace(conn.client, spec)
	ws.release = func() error { return p.projects.put(key, conn) }
	defer func() {
		if lease == nil {
			_ = ws.Close(ctx)
		}
	}()
	if err := prepare(ws); err != nil {
		return nil, ws.discard.Load() && ctx.Err() == nil, err
	}
	return ws, false, nil
}

func (w *workspace) requireRunningProject(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	// NewSession can block before a channel exists to cancel.
	stop := context.AfterFunc(ctx, w.abortTransport)
	status, err := w.status(ctx)
	stop()
	if ctx.Err() != nil {
		w.discard.Store(true)
		return ctx.Err()
	}
	if err == nil && status.State != sandbox.ProjectRunning {
		return sandbox.ErrWorkspaceMissing
	}
	return err
}

func (p *Provider) ProjectStatus(ctx context.Context, spec sandbox.Spec) (sandbox.ProjectStatus, error) {
	ws, status, err := p.openProjectStatus(ctx, spec)
	if err != nil {
		return status, err
	}
	_ = ws.Close(ctx)
	return status, nil
}

func (p *Provider) openProjectStatus(ctx context.Context, spec sandbox.Spec) (*workspace, sandbox.ProjectStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	var status sandbox.ProjectStatus
	ws, err := p.openProject(ctx, spec, func(ws *workspace) error {
		var err error
		status, err = ws.status(ctx)
		return err
	})
	return ws, status, err
}

func (p *Provider) DestroyProject(ctx context.Context, spec sandbox.Spec, expectedID *string) error {
	release, err := provisionGates.Acquire(ctx, spec.ProjectKey(), false)
	if err != nil {
		return err
	}
	defer release()
	ws, status, err := p.openProjectStatus(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close(ctx) }()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close(ctx) })
	defer stop()
	if expectedID != nil && status.ID != *expectedID {
		return sandbox.ErrProjectChanged
	}
	if status.State == sandbox.ProjectNone {
		return nil
	}
	removeErr := ws.docker(ctx, "rm", "--force", "--volumes", "--", status.ID)
	if removeErr == nil {
		return nil
	}
	var remaining sandbox.ProjectStatus
	var checkErr error
	if !ws.discard.Load() {
		remaining, checkErr = ws.status(ctx)
		if checkErr == nil && remaining.State == sandbox.ProjectNone {
			return nil
		}
	}
	if ws.discard.Load() && ctx.Err() == nil {
		// Docker may have removed the container before SSH lost its reply.
		// Recheck on a healthy transport before reporting the outcome.
		stop()
		_ = ws.Close(ctx)
		observed, err := p.ProjectStatus(ctx, spec)
		if err == nil && observed.State == sandbox.ProjectNone {
			return nil
		}
		checkErr = errors.Join(checkErr, err)
	}
	return errors.Join(removeErr, checkErr, ctx.Err())
}

var _ sandbox.ManagedProjectProvider = (*Provider)(nil)
var _ sandbox.ProjectInspector = (*Provider)(nil)
