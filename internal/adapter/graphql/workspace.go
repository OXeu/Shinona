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

func (r *resolver) WorkspaceRoot(ctx context.Context, args workspaceArgs) (string, error) {
	root, err := r.service.WorkspaceRoot(ctx, auth.CurrentUser(ctx).ID, string(args.SessionID))
	if err != nil {
		return "", r.apiError(err)
	}
	return root, nil
}

func (r *resolver) SetWorkspaceRoot(ctx context.Context, args struct {
	SessionID gql.ID
	Root      string
}) (string, error) {
	owner := auth.CurrentUser(ctx).ID
	root := args.Root
	// Validate the directory inside the live lease before saving the choice.
	if root != "" && root != "." {
		err := r.service.InspectWorkspace(ctx, owner, string(args.SessionID), func(ctx context.Context, ws sandbox.Workspace) error {
			return workspace.New(ws).ResolveRoot(ctx, root)
		})
		if err != nil {
			if errors.Is(err, workspace.ErrPath) {
				return "", codedError{run.ErrWorkspaceRoot, "WORKSPACE_INVALID_ROOT"}
			}
			return "", r.workspaceError(err)
		}
	}
	root, err := r.service.SetWorkspaceRoot(ctx, owner, string(args.SessionID), root)
	if err != nil {
		return "", r.apiError(err)
	}
	return root, nil
}

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
		// The files panel browses the whole workspace so it can select any folder.
		return workspace.New(repo.Workspace()).Directory(ctx, args.Path)
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
		// Files browse the whole workspace, so a plain preview stays unscoped.
		return workspace.New(repo.Workspace()).File(ctx, args.Path)
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

// inWorkspace opens the session workspace scoped to the account's selected
// directory, so changes, history, files, and commits stay within it.
func inWorkspace[T any](ctx context.Context, r *resolver, open openWorkspace, id gql.ID, use func(workspace.Repository, context.Context) (T, error)) (result T, err error) {
	owner := auth.CurrentUser(ctx).ID
	root, err := r.service.WorkspaceRoot(ctx, owner, string(id))
	if err != nil {
		return result, r.apiError(err)
	}
	err = open(ctx, owner, string(id), func(ctx context.Context, ws sandbox.Workspace) error {
		var operationErr error
		result, operationErr = use(workspace.New(ws).At(root), ctx)
		return operationErr
	})
	if err != nil {
		return result, r.workspaceError(err)
	}
	return result, nil
}

// workspaceError maps a workspace failure to its stable API error code.
func (r *resolver) workspaceError(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, run.ErrWorkspaceUnavailable):
		return codedError{run.ErrWorkspaceUnavailable, "WORKSPACE_UNAVAILABLE"}
	case errors.Is(err, run.ErrWorkspaceBusy):
		return codedError{run.ErrWorkspaceBusy, "WORKSPACE_BUSY"}
	case errors.Is(err, workspace.ErrRepository):
		return codedError{workspace.ErrRepository, "WORKSPACE_NO_REPOSITORY"}
	case errors.Is(err, workspace.ErrPath):
		return codedError{workspace.ErrPath, "WORKSPACE_INVALID_PATH"}
	case errors.Is(err, workspace.ErrBinary):
		return codedError{workspace.ErrBinary, "WORKSPACE_BINARY_FILE"}
	case errors.Is(err, workspace.ErrCommit):
		return codedError{workspace.ErrCommit, "WORKSPACE_COMMIT_FAILED"}
	default:
		return r.apiError(err)
	}
}
