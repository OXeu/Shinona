package main

import (
	"context"
	"errors"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

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
	number, portErr := strconv.Atoi(port)
	if err != nil || portErr != nil || number < 1 || number > 65535 || host == "" ||
		strings.ContainsAny(host, " /?#@%\r\n\t") {
		return "", errors.New("SSH address must be a host and port")
	}
	return net.JoinHostPort(strings.ToLower(strings.TrimRight(host, ".")), strconv.Itoa(number)), nil
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
	}
	if !r.trustedSSH[server.Address] {
		if err := httpclient.CheckEndpoint("https://" + server.Address); err != nil {
			return nil, settings.Invalid("PRIVATE_ENDPOINT", "the SSH host must use a public address")
		}
		config.DialContext = httpclient.DialPublic
	}
	provider, err := sshsandbox.New(config)
	if err != nil {
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

func (r *runtime) SaveSSHServer(ctx context.Context, server settings.SSHServer) (settings.SSHServer, error) {
	r.sshMu.Lock()
	defer r.sshMu.Unlock()

	owner := auth.CurrentUser(ctx).ID
	server.Name = strings.Join(strings.Fields(server.Name), " ")
	server.User = strings.TrimSpace(server.User)
	server.Template = strings.TrimSpace(server.Template)
	server.HostFingerprint = strings.TrimSpace(server.HostFingerprint)
	server.PrivateKey = strings.TrimSpace(server.PrivateKey)
	if server.Template == "" {
		server.Template = defaultSSHImage
	}
	if server.Name == "" || len(server.Name) > 160 || len(server.User) > 255 || !sshImageName.MatchString(server.Template) {
		return settings.SSHServer{}, settings.Invalid("INVALID_PERSONAL_SSH", "enter a server name and a valid SSH configuration")
	}
	address, err := canonicalSSHAddress(strings.TrimSpace(server.Address))
	if err != nil || len(server.Address) > 255 {
		return settings.SSHServer{}, settings.Invalid("INVALID_PERSONAL_SSH", "SSH address must be a host and port")
	}
	server.Address = address
	create := server.ID == ""
	if create {
		server.ID = uuid.NewString()
	} else {
		previous, err := r.repo.SSHServer(ctx, owner, server.ID)
		if err != nil {
			return settings.SSHServer{}, err
		}
		if server.PrivateKey == "" {
			server.PrivateKey = previous.PrivateKey
		}
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

func (r *runtime) RemoveSSHServer(ctx context.Context, id string) error {
	r.sshMu.Lock()
	defer r.sshMu.Unlock()
	return r.repo.RemoveSSHServer(ctx, auth.CurrentUser(ctx).ID, id)
}
