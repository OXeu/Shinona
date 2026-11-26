package rest

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/settings"
)

func (r *handler) SSHServers(ctx context.Context) ([]settings.SSHServerInfo, error) {
	servers, err := r.app.SSHServers(ctx)
	return nonNil(servers), r.apiError(err)
}

type sshServerInput struct {
	settings.SSHServerInfo
	PrivateKey string `json:"privateKey"`
}

func (input sshServerInput) server() settings.SSHServer {
	return settings.SSHServer{SSHServerInfo: input.SSHServerInfo, PrivateKey: input.PrivateKey}
}

func (r *handler) SaveSSHServer(ctx context.Context, input sshServerInput) (settings.SSHServerInfo, error) {
	server, err := r.app.SaveSSHServer(ctx, input.server())
	return server, r.sshServerError(err)
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
