package main

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/adapter/sshsandbox"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

const defaultSSHImage = "buildpack-deps:bookworm-scm"

var sshImageName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/@:-]{0,254}$`)

func canonicalSSHAddress(raw string) (string, error) {
	host, port, err := net.SplitHostPort(raw)
	host = strings.TrimRight(host, ".")
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || host == "" ||
		strings.ContainsAny(host, " /?#@%\r\n\t") {
		return "", errors.New("SSH address must be a host and port")
	}
	return net.JoinHostPort(strings.ToLower(host), strconv.Itoa(number)), nil
}

func trustedSSHAddresses(raw string) (map[string]bool, error) {
	trusted := make(map[string]bool)
	for _, entry := range strings.Fields(strings.ReplaceAll(raw, ",", " ")) {
		address, err := canonicalSSHAddress(entry)
		if err != nil {
			return nil, errors.New("SHINONA_TRUSTED_SSH_ADDRESSES must list host:port addresses")
		}
		trusted[address] = true
	}
	return trusted, nil
}

func (r *runtime) personalSSH(server settings.SSHServer) (*sshsandbox.Provider, error) {
	config := sshsandbox.Config{
		Address: server.Address, User: server.User, HostFingerprint: server.HostFingerprint,
		Template: server.Template, PrivateKey: server.PrivateKey,
		ProjectConnections: &r.sshProjects,
	}
	if !r.trustedSSH[server.Address] {
		if err := httpclient.CheckEndpoint("https://" + server.Address); err != nil {
			if errors.Is(err, httpclient.ErrPrivateEndpoint) {
				return nil, sshPrivateAddressError()
			}
			return nil, sshAddressError()
		}
		config.DialContext = httpclient.DialPublic
	}
	provider, err := sshsandbox.New(config)
	switch {
	case errors.Is(err, sshsandbox.ErrInvalidFingerprint):
		return nil, settings.Invalid("SSH_INVALID_FINGERPRINT", err.Error())
	case errors.Is(err, sshsandbox.ErrInvalidPrivateKey):
		return nil, settings.Invalid("SSH_INVALID_PRIVATE_KEY", err.Error())
	case errors.Is(err, sshsandbox.ErrEncryptedKey):
		return nil, settings.Invalid("SSH_ENCRYPTED_KEY", err.Error())
	case err != nil:
		return nil, settings.Invalid("INVALID_PERSONAL_SSH", err.Error())
	}
	return provider, nil
}

func (r *runtime) ResolveForAccount(ctx context.Context, owner, name string) (run.Target, error) {
	target := r.Resolve(name)
	id, personal := strings.CutPrefix(name, settings.SSHProviderPrefix)
	if !personal {
		if name != "" || target.Provider != nil {
			return target, nil
		}
		servers, err := r.repo.SSHServers(ctx, owner)
		if err != nil || len(servers) == 0 {
			return target, err
		}
		id = servers[0].ID
	}
	server, err := r.repo.SSHServer(ctx, owner, id)
	if errors.Is(err, settings.ErrSSHServer) {
		return target, nil
	}
	if err != nil {
		return target, err
	}
	target = sshTarget(server)
	target.ResolveAgent = r.resolveAgent
	provider, err := r.personalSSH(server)
	if err != nil {
		return target, err
	}
	target.Provider = provider
	return target, nil
}

func (r *runtime) ProjectTargets(ctx context.Context, owner string) ([]run.Target, error) {
	targets := make([]run.Target, 0, 2)
	for _, name := range []string{"e2b", "ssh"} {
		target := r.Resolve(name)
		if _, ok := target.Provider.(sandbox.ManagedProjectProvider); ok {
			targets = append(targets, target)
		}
	}
	servers, err := r.repo.SSHServers(ctx, owner)
	if err != nil {
		return nil, err
	}
	for _, server := range servers {
		targets = append(targets, sshTarget(server))
	}
	return targets, nil
}

func sshTarget(server settings.SSHServer) run.Target {
	return run.Target{Name: settings.SSHProviderPrefix + server.ID, Label: server.Name,
		Defaults: run.ProjectSandboxConfig{Template: server.Template}}
}

func (r *runtime) SSHServers(ctx context.Context) ([]settings.SSHServer, error) {
	return r.repo.SSHServers(ctx, auth.CurrentUser(ctx).ID)
}

func (r *runtime) sshConnectionSettings(ctx context.Context, owner string, server settings.SSHServer) (settings.SSHServer, error) {
	server.User = strings.TrimSpace(server.User)
	server.Template = strings.TrimSpace(server.Template)
	server.HostFingerprint = strings.TrimSpace(server.HostFingerprint)
	server.PrivateKey = strings.TrimSpace(server.PrivateKey)
	if server.Template == "" {
		server.Template = defaultSSHImage
	}
	if server.User == "" || len(server.User) > 255 || strings.ContainsAny(server.User, "\x00\r\n") {
		return settings.SSHServer{}, settings.Invalid("SSH_INVALID_USER", "enter a valid SSH user")
	}
	address, err := canonicalSSHAddress(strings.TrimSpace(server.Address))
	if err != nil || len(server.Address) > 255 {
		return settings.SSHServer{}, sshAddressError()
	}
	server.Address = address
	if server.ID != "" {
		previous, err := r.repo.SSHServer(ctx, owner, server.ID)
		if err != nil {
			return settings.SSHServer{}, err
		}
		if server.PrivateKey == "" {
			server.PrivateKey = previous.PrivateKey
		}
	}
	return server, nil
}

func (r *runtime) SaveSSHServer(ctx context.Context, server settings.SSHServer) (settings.SSHServer, error) {
	r.sshMu.Lock()
	defer r.sshMu.Unlock()

	owner := auth.CurrentUser(ctx).ID
	server, err := r.sshConnectionSettings(ctx, owner, server)
	if err != nil {
		return settings.SSHServer{}, err
	}
	server.Name = strings.Join(strings.Fields(server.Name), " ")
	if server.Name == "" || len(server.Name) > 160 || !sshImageName.MatchString(server.Template) {
		return settings.SSHServer{}, settings.Invalid("INVALID_PERSONAL_SSH", "enter a server name and a valid Docker image")
	}
	create := server.ID == ""
	if create {
		server.ID = uuid.NewString()
	}
	if _, err := r.personalSSH(server); err != nil {
		return settings.SSHServer{}, err
	}
	if err := r.repo.SaveSSHServer(ctx, owner, server, create); err != nil {
		return settings.SSHServer{}, err
	}
	server.PrivateKey = ""
	return server, nil
}

func (r *runtime) TestSSHServer(ctx context.Context, server settings.SSHServer) error {
	// The connection test uses only the SSH fields, including the saved key when omitted.
	server.Template = defaultSSHImage
	server, err := r.sshConnectionSettings(ctx, auth.CurrentUser(ctx).ID, server)
	if err != nil {
		return err
	}
	provider, err := r.personalSSH(server)
	if err != nil {
		return err
	}
	return sshTestError(provider.TestConnection(ctx))
}

func sshAddressError() error {
	return settings.Invalid("SSH_INVALID_ADDRESS", "SSH address must be a host and port")
}

func sshPrivateAddressError() error {
	return settings.Invalid("SSH_PRIVATE_ADDRESS", "the SSH address is private and has not been allowed by the operator")
}

func sshTestError(err error) error {
	if err == nil {
		return nil
	}
	network, _ := errors.AsType[net.Error](err)
	timedOut := errors.Is(err, context.DeadlineExceeded) || network != nil && network.Timeout()
	var dns *net.DNSError
	var algorithms *ssh.AlgorithmNegotiationError
	switch {
	case errors.Is(err, sshsandbox.ErrDockerUnavailable) && timedOut:
		return settings.Invalid("SSH_DOCKER_TIMEOUT", "SSH connected, but the Docker check timed out")
	case errors.Is(err, sshsandbox.ErrDockerUnavailable):
		return settings.Invalid("SSH_DOCKER_UNAVAILABLE", "SSH connected, but Docker is unavailable to the SSH user")
	case errors.Is(err, sshsandbox.ErrHostKeyMismatch):
		return settings.Invalid("SSH_HOST_KEY_MISMATCH", sshsandbox.ErrHostKeyMismatch.Error())
	case errors.Is(err, sshsandbox.ErrAuthentication):
		return settings.Invalid("SSH_AUTHENTICATION_FAILED", sshsandbox.ErrAuthentication.Error())
	case errors.Is(err, httpclient.ErrPrivateEndpoint):
		return sshPrivateAddressError()
	case errors.As(err, &dns) && !timedOut:
		return settings.Invalid("SSH_DNS_FAILED", "could not resolve the SSH hostname")
	case timedOut:
		return settings.Invalid("SSH_CONNECTION_TIMEOUT", "the SSH connection timed out")
	case errors.As(err, &algorithms):
		return settings.Invalid("SSH_ALGORITHM_MISMATCH", "the SSH client and server have no compatible algorithms")
	case errors.Is(err, sshsandbox.ErrHandshake):
		return settings.Invalid("SSH_HANDSHAKE_FAILED", "the server accepted TCP but the SSH handshake failed")
	case errors.Is(err, syscall.ECONNREFUSED):
		return settings.Invalid("SSH_CONNECTION_REFUSED", "the SSH port refused the connection")
	case errors.Is(err, syscall.ENETUNREACH) || errors.Is(err, syscall.EHOSTUNREACH):
		return settings.Invalid("SSH_HOST_UNREACHABLE", "the SSH host is unreachable from the application server")
	default:
		return settings.Invalid("SSH_CONNECTION_FAILED", "could not open a TCP connection to the SSH host")
	}
}

func (r *runtime) RemoveSSHServer(ctx context.Context, id string) error {
	r.sshMu.Lock()
	defer r.sshMu.Unlock()
	return r.repo.RemoveSSHServer(ctx, auth.CurrentUser(ctx).ID, id)
}
