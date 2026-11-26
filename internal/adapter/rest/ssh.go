package rest

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/settings"
)

type sshServerResponse struct {
	ID              string `json:"id"`
	Name            string `json:"name"`
	Address         string `json:"address"`
	User            string `json:"user"`
	HostFingerprint string `json:"hostFingerprint"`
	Template        string `json:"template"`
}

func sshServer(server settings.SSHServer) sshServerResponse {
	return sshServerResponse{
		ID: server.ID, Name: server.Name, Address: server.Address, User: server.User,
		HostFingerprint: server.HostFingerprint, Template: server.Template,
	}
}

func (r *handler) SSHServers(ctx context.Context) ([]sshServerResponse, error) {
	servers, err := r.app.SSHServers(ctx)
	if err != nil {
		return nil, r.apiError(err)
	}
	result := make([]sshServerResponse, len(servers))
	for i, server := range servers {
		result[i] = sshServer(server)
	}
	return result, nil
}

type sshServerInput struct {
	ID                                                         string
	Name, Address, User, HostFingerprint, Template, PrivateKey string
}

func (input sshServerInput) server() settings.SSHServer {
	return settings.SSHServer{
		ID: input.ID, Name: input.Name, Address: input.Address, User: input.User,
		HostFingerprint: input.HostFingerprint, Template: input.Template, PrivateKey: input.PrivateKey,
	}
}

func (r *handler) SaveSSHServer(ctx context.Context, input sshServerInput) (sshServerResponse, error) {
	server, err := r.app.SaveSSHServer(ctx, input.server())
	return sshServer(server), r.sshServerError(err)
}

func (r *handler) TestSSHServer(ctx context.Context, input sshServerInput) error {
	err := r.app.TestSSHServer(ctx, input.server())
	return r.sshServerError(err)
}

func (r *handler) sshServerError(err error) error {
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		return codedError{err, invalid.Code}
	}
	return r.apiError(err)
}

func (r *handler) RemoveSSHServer(ctx context.Context, args resourceID) error {
	err := r.app.RemoveSSHServer(ctx, args.ID)
	return r.apiError(err)
}
