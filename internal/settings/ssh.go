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

type SSHServerInfo struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	User            string `json:"user"`
	HostFingerprint string `json:"hostFingerprint"`
	Template        string `json:"template"`
}

type SSHServer struct {
	SSHServerInfo
	PrivateKey string `json:"-"`
}

type SSHRepository interface {
	SSHServers(context.Context, string) ([]SSHServerInfo, error)
	DefaultSSHServer(ctx context.Context, owner string) (SSHServer, error)
	SSHServer(ctx context.Context, owner, id string) (SSHServer, error)
	// A blank private key keeps the stored key on updates.
	SaveSSHServer(ctx context.Context, owner string, server SSHServer, create bool) error
	RemoveSSHServer(ctx context.Context, owner, id string) error
}
