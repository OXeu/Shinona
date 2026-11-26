package graphql

import (
	"context"
	"errors"

	gql "github.com/graph-gophers/graphql-go"

	"github.com/OXeu/shinona/internal/settings"
)

type sshServerResolver struct {
	ID              gql.ID
	Name            string
	Address         string
	User            string
	HostFingerprint string
	Template        string
}

func sshServer(server settings.SSHServer) sshServerResolver {
	return sshServerResolver{
		ID: gql.ID(server.ID), Name: server.Name, Address: server.Address, User: server.User,
		HostFingerprint: server.HostFingerprint, Template: server.Template,
	}
}

func (r *resolver) SSHServers(ctx context.Context) ([]sshServerResolver, error) {
	servers, err := r.app.SSHServers(ctx)
	if err != nil {
		return nil, r.apiError(err)
	}
	result := make([]sshServerResolver, len(servers))
	for i, server := range servers {
		result[i] = sshServer(server)
	}
	return result, nil
}

func (r *resolver) SaveSSHServer(ctx context.Context, args struct {
	Input struct {
		ID                                                         gql.ID
		Name, Address, User, HostFingerprint, Template, PrivateKey string
	}
}) (sshServerResolver, error) {
	input := args.Input
	server, err := r.app.SaveSSHServer(ctx, settings.SSHServer{
		ID: string(input.ID), Name: input.Name, Address: input.Address, User: input.User,
		HostFingerprint: input.HostFingerprint, Template: input.Template, PrivateKey: input.PrivateKey,
	})
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		return sshServerResolver{}, codedError{err, invalid.Code}
	}
	return sshServer(server), r.apiError(err)
}

func (r *resolver) RemoveSSHServer(ctx context.Context, args struct{ ID gql.ID }) (bool, error) {
	err := r.app.RemoveSSHServer(ctx, string(args.ID))
	return err == nil, r.apiError(err)
}
