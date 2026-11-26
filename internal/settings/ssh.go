package settings

import (
	"context"
	"errors"
)

const SSHProviderPrefix = "ssh:"

var (
	ErrSSHServer      = errors.New("SSH server not found")
	ErrSSHServerInUse = errors.New("the SSH server is assigned to a project")
	ErrSSHServerName  = errors.New("an SSH server with this name already exists")
)

type SSHServer struct {
	ID              string
	Name            string
	Address         string
	User            string
	HostFingerprint string
	Template        string
	PrivateKey      string
}

type SSHRepository interface {
	SSHServers(context.Context, string) ([]SSHServer, error)
	SSHServer(ctx context.Context, owner, id string) (SSHServer, error)
	SaveSSHServer(ctx context.Context, owner string, server SSHServer, create bool) error
	RemoveSSHServer(ctx context.Context, owner, id string) error
}
