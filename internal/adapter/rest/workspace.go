package rest

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
	"github.com/OXeu/shinona/internal/workspace"
)

type workspaceArgs struct {
	SessionID string `uri:"id" json:"-"`
}

func (r *handler) WorkspaceRoot(ctx context.Context, args workspaceArgs) (string, error) {
	root, err := r.service.WorkspaceRoot(ctx, auth.CurrentUser(ctx).ID, args.SessionID)
	if err != nil {
		return "", r.apiError(err)
	}
	return root, nil
}

func (r *handler) SetWorkspaceRoot(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	Root      string
}) (string, error) {
	owner := auth.CurrentUser(ctx).ID
	root, err := r.service.SetWorkspaceRoot(ctx, owner, args.SessionID, args.Root, func(ctx context.Context, root string) error {
		return r.service.InspectWorkspace(ctx, owner, args.SessionID, func(ctx context.Context, ws sandbox.Workspace) error {
			return workspace.New(ws).ResolveRoot(ctx, root)
		})
	})
	if errors.Is(err, workspace.ErrPath) || errors.Is(err, run.ErrWorkspaceRoot) {
		return "", codedError{run.ErrWorkspaceRoot, "WORKSPACE_INVALID_ROOT"}
	}
	return root, r.workspaceError(err)
}

func (r *handler) WorkspaceChanges(ctx context.Context, args workspaceArgs) (workspace.Changes, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, workspace.Repository.Changes)
}

func (r *handler) WorkspaceHistory(ctx context.Context, args workspaceArgs) (workspace.History, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, workspace.Repository.History)
}

func (r *handler) WorkspaceDirectory(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	Path      string `form:"path"`
}) (workspace.Directory, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (workspace.Directory, error) {
		defer timing.Start(ctx, "workspace_directory")()
		// The files panel browses the whole workspace so it can select any folder.
		return workspace.New(repo.Workspace()).Directory(ctx, args.Path)
	})
}

func (r *handler) WorkspacePreview(ctx context.Context, args struct {
	SessionID string  `uri:"id" json:"-"`
	Path      string  `form:"path"`
	Diff      bool    `form:"diff"`
	Revision  *string `form:"revision"`
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

func (r *handler) CommitWorkspace(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	Message   string
	Paths     []string
}) (string, error) {
	user := auth.CurrentUser(ctx)
	return inWorkspace(ctx, r, r.service.WithWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (string, error) {
		return repo.Commit(ctx, user.Email, args.Message, args.Paths)
	})
}

type openWorkspace func(context.Context, string, string, func(context.Context, sandbox.Workspace) error) error

// inWorkspace opens the session workspace with the project's working directory
// for Git operations. File browsing can use the underlying workspace directly.
func inWorkspace[T any](ctx context.Context, r *handler, open openWorkspace, id string, use func(workspace.Repository, context.Context) (T, error)) (result T, err error) {
	owner := auth.CurrentUser(ctx).ID
	root, err := r.service.WorkspaceRoot(ctx, owner, id)
	if err != nil {
		return result, r.apiError(err)
	}
	err = open(ctx, owner, id, func(ctx context.Context, ws sandbox.Workspace) error {
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
func (r *handler) workspaceError(err error) error {
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
