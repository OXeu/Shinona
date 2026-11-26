// Package sshsandbox manages Docker-backed project workspaces on an SSH host.
package sshsandbox

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/semaphore"

	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	connectTimeout   = 10 * time.Second
	provisionTimeout = 2 * time.Minute
	workDirectory    = "/workspace"
	maxFileBytes     = 64 << 10
	maxOutputBytes   = 16 << 10
)

type Config struct {
	Address         string
	User            string
	KeyPath         string
	HostFingerprint string
	Template        string
	PrivateKey      string
	DialContext     func(context.Context, string, string) (net.Conn, error)
}

type Provider struct {
	address  string
	client   *ssh.ClientConfig
	template string
	dial     func(context.Context, string, string) (net.Conn, error)
}

var provisionGates = func() [64]*semaphore.Weighted {
	var gates [64]*semaphore.Weighted
	for i := range gates {
		gates[i] = semaphore.NewWeighted(1)
	}
	return gates
}()

func projectHash(spec sandbox.Spec) [sha256.Size]byte {
	return sha256.Sum256([]byte(spec.OwnerID + "\x00" + spec.ProjectID))
}

func provisionGate(spec sandbox.Spec) *semaphore.Weighted {
	return provisionGates[int(projectHash(spec)[0])%len(provisionGates)]
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
		return nil, errors.New("SSH requires a SHA256 host key fingerprint")
	}
	key, err := privateKey(config)
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(key)
	if err != nil {
		return nil, errors.New("SSH requires a valid private key without a passphrase")
	}
	client := &ssh.ClientConfig{
		User: config.User, Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if ssh.FingerprintSHA256(key) != fingerprint {
				return errors.New("SSH host key fingerprint does not match")
			}
			return nil
		},
	}
	dial := config.DialContext
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	return &Provider{address: config.Address, client: client, template: config.Template, dial: dial}, nil
}

func privateKey(config Config) ([]byte, error) {
	if config.KeyPath == "" {
		if config.PrivateKey == "" || len(config.PrivateKey) > maxFileBytes {
			return nil, errors.New("SSH requires a private key of at most 64 KiB")
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
	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()
	conn, err := p.dial(ctx, "tcp", p.address)
	if err != nil {
		return nil, fmt.Errorf("dial the sandbox SSH host: %w", err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	client, channels, requests, err := ssh.NewClientConn(conn, p.address, p.client)
	if !stop() || err != nil {
		_ = conn.Close()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("authenticate to the sandbox SSH host: %w", err)
	}
	return &workspace{
		client: ssh.NewClient(client, channels, requests),
		name:   fmt.Sprintf("shinona-%x", projectHash(spec)),
	}, nil
}

func (*Provider) Policies(string) []sandbox.Policy { return []sandbox.Policy{sandbox.Shared} }

func (p *Provider) Acquire(ctx context.Context, spec sandbox.Spec) (sandbox.Workspace, error) {
	if spec.Policy != sandbox.Shared {
		return nil, sandbox.ErrPolicy
	}
	ctx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()
	gate := provisionGate(spec)
	if err := gate.Acquire(ctx, 1); err != nil {
		return nil, err
	}
	defer gate.Release(1)
	ws, err := p.connect(ctx, spec)
	if err != nil {
		return nil, err
	}
	if err := p.ensureRunning(ctx, ws, spec); err != nil {
		_ = ws.Close(ctx)
		return nil, err
	}
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
	ws, err := p.connect(ctx, spec)
	if err != nil {
		return nil, err
	}
	status, err := ws.status(ctx)
	if err == nil && status.State == sandbox.ProjectRunning {
		return ws, nil
	}
	_ = ws.Close(ctx)
	if err != nil {
		return nil, err
	}
	return nil, sandbox.ErrWorkspaceMissing
}

func (p *Provider) ProjectStatus(ctx context.Context, spec sandbox.Spec) (sandbox.ProjectStatus, error) {
	ws, err := p.connect(ctx, spec)
	if err != nil {
		return sandbox.ProjectStatus{}, err
	}
	defer func() { _ = ws.Close(ctx) }()
	return ws.status(ctx)
}

func (p *Provider) DestroyProject(ctx context.Context, spec sandbox.Spec) error {
	gate := provisionGate(spec)
	if err := gate.Acquire(ctx, 1); err != nil {
		return err
	}
	defer gate.Release(1)
	ws, err := p.connect(ctx, spec)
	if err != nil {
		return err
	}
	defer func() { _ = ws.Close(ctx) }()
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
