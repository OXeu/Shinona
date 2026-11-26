package run

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

var (
	ErrWorkspaceUnavailable = errors.New("this session has no retained workspace")
	ErrWorkspaceBusy        = errors.New("the workspace is busy")
)

const workspaceTimeout = 15 * time.Second

type liveWorkspace struct {
	workspace sandbox.Workspace
	ctx       context.Context
	projectID *string // Nil denotes a session-scoped lease. An empty ID denotes the default project.
	readers   sync.WaitGroup
}

func (c *Controls) publishWorkspace(active *activeTurn, ws sandbox.Workspace, projectID *string) func() {
	ctx, cancel := context.WithCancel(active.ctx)
	lease := &liveWorkspace{workspace: ws, ctx: ctx, projectID: projectID}
	c.mu.Lock()
	active.workspace = lease
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		active.workspace = nil
		c.mu.Unlock()
		cancel()
		// Drain borrowed readers before the worker closes the provider's lease.
		lease.readers.Wait()
	}
}

func (s *Service) borrowWorkspace(ctx context.Context, owner, id string) (*liveWorkspace, error) {
	c := s.controls
	c.mu.Lock()
	active := c.active
	if active == nil || active.item.OwnerID != owner || active.workspace == nil || active.workspace.ctx.Err() != nil {
		c.mu.Unlock()
		return nil, nil
	}
	live := active.workspace
	if active.item.SessionID == id {
		live.readers.Add(1)
		c.mu.Unlock()
		return live, nil
	}
	c.mu.Unlock()
	if live.projectID == nil {
		return nil, nil
	}
	projectID, err := s.repo.SessionProject(ctx, owner, id)
	if errors.Is(err, ErrSession) {
		return nil, ErrWorkspaceUnavailable
	}
	if err != nil || projectID != *live.projectID {
		return nil, err
	}

	// The worker may have detached the lease while we verified membership.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != active || active.workspace != live || live.ctx.Err() != nil {
		return nil, nil
	}
	live.readers.Add(1)
	return live, nil
}

// InspectWorkspace can borrow a live lease for the same project. Between
// turns, several readers may open the retained workspace concurrently.
func (s *Service) InspectWorkspace(ctx context.Context, owner, id string, inspect func(context.Context, sandbox.Workspace) error) error {
	ctx, cancel := context.WithTimeout(ctx, workspaceTimeout)
	defer cancel()
	live, err := s.borrowWorkspace(ctx, owner, id)
	if err != nil {
		return err
	}
	if live == nil {
		return s.withWorkspace(ctx, owner, id, true, inspect)
	}
	defer live.readers.Done()
	stop := context.AfterFunc(live.ctx, cancel)
	defer stop()
	err = inspect(ctx, live.workspace)
	if errors.Is(err, context.Canceled) && live.ctx.Err() != nil {
		return ErrWorkspaceBusy
	}
	return err
}

// WithWorkspace shares the lease gate with agent execution. It reopens the
// retained workspace for an account's session or project.
func (s *Service) WithWorkspace(ctx context.Context, owner, id string, use func(context.Context, sandbox.Workspace) error) error {
	return s.withWorkspace(ctx, owner, id, false, use)
}

func (s *Service) withWorkspace(ctx context.Context, owner, id string, readOnly bool, use func(context.Context, sandbox.Workspace) error) (err error) {
	if readOnly {
		if !s.controls.leaseMu.TryRLock() {
			return ErrWorkspaceBusy
		}
		defer s.controls.leaseMu.RUnlock()
	} else {
		if !s.controls.leaseMu.TryLock() {
			return ErrWorkspaceBusy
		}
		defer s.controls.leaseMu.Unlock()
	}
	ctx, cancel := context.WithTimeout(ctx, workspaceTimeout)
	defer cancel()
	acquired := sync.OnceFunc(timing.Start(ctx, "workspace_acquire"))
	defer acquired()
	resolve := timing.Start(ctx, "workspace_resolve")
	spec, target, err := sessionSandbox(ctx, s.repo, s.resolve, owner, id, "", false)
	resolve()
	if errors.Is(err, ErrSession) {
		return ErrWorkspaceUnavailable
	}
	if err != nil {
		return err
	}
	spec.RunID = s.newID()
	if inspector, ok := target.Provider.(sandbox.ProjectInspector); readOnly && ok {
		return workspaceAvailability(inspector.InspectProject(ctx, spec, func(ctx context.Context, ws sandbox.Workspace) error {
			acquired()
			return use(ctx, ws)
		}))
	}
	ws, err := s.openWorkspace(ctx, spec, target)
	acquired()
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		release := timing.Start(ctx, "workspace_release")
		closeErr := ws.Close(cleanup)
		release()
		// Keep an acknowledged operation successful even if lease cleanup fails.
		if err != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return use(ctx, ws)
}

func workspaceAvailability(err error) error {
	if errors.Is(err, sandbox.ErrWorkspaceMissing) {
		return ErrWorkspaceUnavailable
	}
	return err
}

func (s *Service) openWorkspace(ctx context.Context, spec sandbox.Spec, target Target) (sandbox.Workspace, error) {
	if project, ok := target.Provider.(sandbox.ProjectProvider); ok {
		ws, err := project.OpenProject(ctx, spec)
		return ws, workspaceAvailability(err)
	}
	turn, err := s.repo.LatestWorkspaceRun(ctx, spec.OwnerID, spec.SessionID)
	if err != nil {
		return nil, err
	}
	if turn == nil || turn.Backend == "" || turn.Policy != sandbox.Shared {
		return nil, ErrWorkspaceUnavailable
	}
	target, err = s.resolve(ctx, spec.OwnerID, turn.Backend)
	if err != nil {
		return nil, err
	}
	retained, ok := target.Provider.(sandbox.RetainedProvider)
	if !ok {
		return nil, ErrWorkspaceUnavailable
	}
	return retained.OpenRetained(ctx, spec)
}
