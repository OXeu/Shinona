package graphql

import (
	"context"
	"errors"

	gql "github.com/graph-gophers/graphql-go"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/workspace"
)

type workspaceArgs struct{ SessionID gql.ID }

func (r *resolver) WorkspaceChanges(ctx context.Context, args workspaceArgs) (workspace.Changes, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, workspace.Repository.Changes)
}

func (r *resolver) WorkspaceHistory(ctx context.Context, args workspaceArgs) (workspace.History, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, workspace.Repository.History)
}

func (r *resolver) WorkspaceDirectory(ctx context.Context, args struct {
	SessionID gql.ID
	Path      string
}) (workspace.Directory, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (workspace.Directory, error) {
		return repo.Directory(ctx, args.Path)
	})
}

func (r *resolver) WorkspacePreview(ctx context.Context, args struct {
	SessionID gql.ID
	Path      string
	Diff      bool
	Revision  *string
}) (workspace.Preview, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (workspace.Preview, error) {
		if args.Diff {
			revision := ""
			if args.Revision != nil {
				revision = *args.Revision
			}
			return repo.Diff(ctx, args.Path, revision)
		}
		return repo.File(ctx, args.Path)
	})
}

func (r *resolver) CommitWorkspace(ctx context.Context, args struct {
	SessionID gql.ID
	Message   string
	Paths     []string
}) (string, error) {
	user := auth.CurrentUser(ctx)
	return inWorkspace(ctx, r, r.service.WithWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (string, error) {
		return repo.Commit(ctx, user.Email, args.Message, args.Paths)
	})
}

type openWorkspace func(context.Context, string, string, func(context.Context, sandbox.Workspace) error) error

func inWorkspace[T any](ctx context.Context, r *resolver, open openWorkspace, id gql.ID, use func(workspace.Repository, context.Context) (T, error)) (result T, err error) {
	err = open(ctx, auth.CurrentUser(ctx).ID, string(id), func(ctx context.Context, ws sandbox.Workspace) error {
		var operationErr error
		result, operationErr = use(workspace.New(ws), ctx)
		return operationErr
	})
	switch {
	case err == nil:
		return result, nil
	case errors.Is(err, run.ErrWorkspaceUnavailable):
		return result, codedError{run.ErrWorkspaceUnavailable, "WORKSPACE_UNAVAILABLE"}
	case errors.Is(err, run.ErrWorkspaceBusy):
		return result, codedError{run.ErrWorkspaceBusy, "WORKSPACE_BUSY"}
	case errors.Is(err, workspace.ErrRepository):
		return result, codedError{workspace.ErrRepository, "WORKSPACE_NO_REPOSITORY"}
	case errors.Is(err, workspace.ErrPath):
		return result, codedError{workspace.ErrPath, "WORKSPACE_INVALID_PATH"}
	case errors.Is(err, workspace.ErrBinary):
		return result, codedError{workspace.ErrBinary, "WORKSPACE_BINARY_FILE"}
	case errors.Is(err, workspace.ErrCommit):
		return result, codedError{workspace.ErrCommit, "WORKSPACE_COMMIT_FAILED"}
	default:
		return result, r.apiError(err)
	}
}
