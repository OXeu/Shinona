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

type workspaceSelection struct {
	ProjectID string `json:"projectId"`
	Root      string `json:"root"`
}

type workspaceChangesResponse struct {
	workspace.Changes
	Selection workspaceSelection `json:"selection"`
}

var errWorkspaceSelection = codedError{errors.New("the workspace selection changed, review the current changes"), "WORKSPACE_SELECTION_CHANGED"}

func (r *handler) WorkspaceRoot(ctx context.Context, args workspaceArgs) (string, error) {
	selection, err := r.service.WorkspaceSelection(ctx, auth.CurrentUser(ctx).ID, args.SessionID)
	return selection.Root, r.apiError(err)
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

func (r *handler) WorkspaceChanges(ctx context.Context, args workspaceArgs) (workspaceChangesResponse, error) {
	return inRepository(ctx, r, r.service.InspectWorkspace, args.SessionID,
		func(ctx context.Context, repo workspace.Repository, selection run.WorkspaceSelection) (workspaceChangesResponse, error) {
			changes, err := repo.Changes(ctx)
			return workspaceChangesResponse{Changes: changes, Selection: workspaceSelection(selection)}, err
		})
}

func (r *handler) WorkspaceHistory(ctx context.Context, args workspaceArgs) (workspace.History, error) {
	return inRepository(ctx, r, r.service.InspectWorkspace, args.SessionID,
		func(ctx context.Context, repo workspace.Repository, _ run.WorkspaceSelection) (workspace.History, error) {
			return repo.History(ctx)
		})
}

func (r *handler) WorkspaceDirectory(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	Path      string `form:"path"`
}) (workspace.Directory, error) {
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (workspace.Directory, error) {
		defer timing.Start(ctx, "workspace_directory")()
		return repo.Directory(ctx, args.Path)
	})
}

func (r *handler) WorkspacePreview(ctx context.Context, args struct {
	SessionID string  `uri:"id" json:"-"`
	Path      string  `form:"path"`
	Diff      bool    `form:"diff"`
	Revision  *string `form:"revision"`
}) (workspace.Preview, error) {
	if args.Diff {
		revision := ""
		if args.Revision != nil {
			revision = *args.Revision
		}
		return inRepository(ctx, r, r.service.InspectWorkspace, args.SessionID,
			func(ctx context.Context, repo workspace.Repository, _ run.WorkspaceSelection) (workspace.Preview, error) {
				return repo.Diff(ctx, args.Path, revision)
			})
	}
	return inWorkspace(ctx, r, r.service.InspectWorkspace, args.SessionID, func(repo workspace.Repository, ctx context.Context) (workspace.Preview, error) {
		return repo.File(ctx, args.Path)
	})
}

func (r *handler) CommitWorkspace(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	Message   string
	Paths     []string
	Selection *workspaceSelection
}) (string, error) {
	if args.Selection == nil {
		return "", errWorkspaceSelection
	}
	user := auth.CurrentUser(ctx)
	return inRepository(ctx, r, r.service.WithWorkspace, args.SessionID,
		func(ctx context.Context, repo workspace.Repository, selection run.WorkspaceSelection) (string, error) {
			if selection != run.WorkspaceSelection(*args.Selection) {
				return "", errWorkspaceSelection
			}
			return repo.Commit(ctx, user.Email, args.Message, args.Paths)
		})
}

type openWorkspace func(context.Context, string, string, func(context.Context, sandbox.Workspace) error) error

// inRepository reads the working directory while the project lease is held.
// Removing a project can move its sessions to the default workspace.
func inRepository[T any](ctx context.Context, r *handler, open openWorkspace, id string,
	use func(context.Context, workspace.Repository, run.WorkspaceSelection) (T, error),
) (T, error) {
	return inWorkspace(ctx, r, open, id, func(repo workspace.Repository, ctx context.Context) (T, error) {
		selection, err := r.service.WorkspaceSelection(ctx, auth.CurrentUser(ctx).ID, id)
		if err != nil {
			var result T
			return result, err
		}
		return use(ctx, repo.At(selection.Root), selection)
	})
}

func inWorkspace[T any](ctx context.Context, r *handler, open openWorkspace, id string, use func(workspace.Repository, context.Context) (T, error)) (result T, err error) {
	err = open(ctx, auth.CurrentUser(ctx).ID, id, func(ctx context.Context, ws sandbox.Workspace) error {
		var operationErr error
		result, operationErr = use(workspace.New(ws), ctx)
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
	case errors.Is(err, errWorkspaceSelection):
		return errWorkspaceSelection
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
