// Package sshsandbox manages Docker-backed project workspaces on an SSH host.
package sshsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/http/httptrace"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	connectTimeout   = 10 * time.Second
	provisionTimeout = 2 * time.Minute
	workDirectory    = "/workspace"
	maxFileBytes     = 64 << 10
	maxOutputBytes   = 16 << 10
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
	// Providers sharing ProjectConnections must use the same dial policy.
	ProjectConnections *ProjectConnections
}

type Provider struct {
	address  string
	client   *ssh.ClientConfig
	template string
	dial     func(context.Context, string, string) (net.Conn, error)
	projects *ProjectConnections
	identity [sha256.Size]byte
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
	identity := sha256.Sum256(ssh.Marshal(struct {
		Address, User, HostKey, PublicKey string
	}{config.Address, config.User, fingerprint, string(signer.PublicKey().Marshal())}))
	return &Provider{
		address: config.Address, client: client, template: config.Template, dial: dial,
		projects: projects, identity: identity,
	}, nil
}

func privateKey(config Config) ([]byte, error) {
	if config.KeyPath == "" {
		if config.PrivateKey == "" || len(config.PrivateKey) > maxFileBytes {
			return nil, ErrInvalidPrivateKey
		}
		return []byte(config.PrivateKey), nil
	}
	file, err := os.Stat(config.KeyPath)
	if err != nil || !file.Mode().IsRegular() || file.Size() > maxFileBytes {
		return nil, errors.New("SSH private key path must point to a readable key file")
	}
	key, err := os.ReadFile(config.KeyPath)
	if err != nil {
		return nil, errors.New("could not read the SSH private key file")
	}
	return key, nil
}

func (p *Provider) connect(ctx context.Context, spec sandbox.Spec) (*workspace, error) {
	client, err := p.connectClient(ctx)
	if err != nil {
		return nil, err
	}
	return newWorkspace(client, spec), nil
}

func (p *Provider) connectClient(ctx context.Context) (*ssh.Client, error) {
	defer timing.Start(ctx, "ssh_connect")()
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	dialed := timing.Start(ctx, "ssh_dial")
	conn, err := p.dial(sshDialTrace(ctx), "tcp", p.address)
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

func sshDialTrace(ctx context.Context) context.Context {
	var mu sync.Mutex
	var dns time.Time
	attempts := make(map[string]time.Time)
	return httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			mu.Lock()
			defer mu.Unlock()
			dns = time.Now()
		},
		DNSDone: func(httptrace.DNSDoneInfo) {
			mu.Lock()
			defer mu.Unlock()
			if !dns.IsZero() {
				timing.Since(ctx, "ssh_dns", dns)
			}
		},
		ConnectStart: func(_, address string) {
			mu.Lock()
			defer mu.Unlock()
			attempts[address] = time.Now()
		},
		ConnectDone: func(_, address string, _ error) {
			mu.Lock()
			defer mu.Unlock()
			if start, ok := attempts[address]; ok {
				timing.Since(ctx, "ssh_tcp", start)
				delete(attempts, address)
			}
		},
	})
}

func newWorkspace(client *ssh.Client, spec sandbox.Spec) *workspace {
	return &workspace{client: client, name: fmt.Sprintf("shinona-%x", projectHash(spec))}
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
	if err := ws.docker(ctx, "info", "--format", "{{.ServerVersion}}"); err != nil {
		return errors.Join(ErrDockerUnavailable, ctx.Err(), err)
	}
	return nil
}

func (p *Provider) Acquire(ctx context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	if spec.Policy != sandbox.Shared {
		return nil, sandbox.ErrPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	release, err := provisionGates.Acquire(ctx, spec.ProjectKey(), false)
	if err != nil {
		return nil, err
	}
	defer release()
	ws, err := p.openProject(ctx, spec, func(ws *workspace) error {
		stop := context.AfterFunc(ctx, func() { _ = ws.client.Close() })
		defer stop()
		return p.ensureRunning(ctx, ws, spec)
	})
	if err != nil {
		return nil, err
	}
	ws.shared = true
	return ws, nil
}

func (p *Provider) ensureRunning(ctx context.Context, ws *workspace, spec sandbox.Spec) error {
	status, err := ws.status(ctx)
	if err != nil {
		return err
	}
	switch status.State {
	case sandbox.ProjectRunning:
		return nil
	case sandbox.ProjectPaused:
		return ws.docker(ctx, "unpause", ws.name)
	case sandbox.ProjectStopped:
		return ws.docker(ctx, "start", ws.name)
	}
	template := spec.Template
	if template == "" {
		template = p.template
	}
	err = ws.docker(ctx, "run", "--detach", "--init", "--security-opt", "no-new-privileges",
		"--name", ws.name, "--user", "0:0", "--workdir", workDirectory,
		"--entrypoint", "/bin/sh", "--", template, "-c", "exec sleep infinity")
	if err == nil {
		status, err = ws.status(ctx)
		if err == nil && status.State != sandbox.ProjectRunning {
			err = errors.New("SSH sandbox image did not keep its container running")
		}
	}
	return err
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
		// Preparation can be a read or a provisioning step that checks current
		// state before changing the deterministically named container.
		ws, _, err = p.tryProject(ctx, spec, key, nil, prepare)
	}
	return ws, err
}

func (p *Provider) tryProject(ctx context.Context, spec sandbox.Spec, key projectConnectionKey, conn *projectConnection, prepare func(*workspace) error) (lease *workspace, retry bool, err error) {
	reused := conn != nil
	if !reused {
		client, err := p.connectClient(ctx)
		if err != nil {
			return nil, false, err
		}
		conn = &projectConnection{client: client, opened: time.Now()}
	}
	ws := newWorkspace(conn.client, spec)
	ws.release = func() error { return p.projects.put(key, conn) }
	defer func() {
		if lease == nil {
			_ = ws.Close(ctx)
		}
	}()
	if err := prepare(ws); err != nil {
		return nil, reused && ws.discard.Load() && ctx.Err() == nil, err
	}
	return ws, false, nil
}

func (w *workspace) requireRunningProject(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	// NewSession can block before a channel exists to cancel.
	stop := context.AfterFunc(ctx, func() { _ = w.client.Close() })
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
	ws, err := p.connect(ctx, spec)
	if err != nil {
		return sandbox.ProjectStatus{}, err
	}
	defer func() { _ = ws.Close(ctx) }()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close(ctx) })
	defer stop()
	return ws.status(ctx)
}

func (p *Provider) DestroyProject(ctx context.Context, spec sandbox.Spec) error {
	release, err := provisionGates.Acquire(ctx, spec.ProjectKey(), false)
	if err != nil {
		return err
	}
	defer release()
	ws, err := p.connect(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close(ctx) }()
	stop := context.AfterFunc(ctx, func() { _ = ws.Close(ctx) })
	defer stop()
	status, err := ws.status(ctx)
	if err != nil || status.State == sandbox.ProjectNone {
		return err
	}
	if err := ws.docker(ctx, "rm", "--force", "--volumes", ws.name); err != nil {
		if status, checkErr := ws.status(ctx); checkErr != nil || status.State != sandbox.ProjectNone {
			return err
		}
	}
	return nil
}

var _ sandbox.ManagedProjectProvider = (*Provider)(nil)
var _ sandbox.ProjectInspector = (*Provider)(nil)
