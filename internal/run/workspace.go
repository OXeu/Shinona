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
	readers   sync.WaitGroup
}

func (c *Controls) publishWorkspace(active *activeTurn, ws sandbox.Workspace) func() {
	ctx, cancel := context.WithCancel(active.ctx)
	lease := &liveWorkspace{workspace: ws, ctx: ctx}
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

func (c *Controls) borrowWorkspace(owner, id string) *liveWorkspace {
	c.mu.Lock()
	defer c.mu.Unlock()
	active := c.active
	if active == nil || active.item.OwnerID != owner || active.item.SessionID != id ||
		active.workspace == nil || active.workspace.ctx.Err() != nil {
		return nil
	}
	active.workspace.readers.Add(1)
	return active.workspace
}

// InspectWorkspace runs a read-only operation on the live lease when available.
// A retained workspace can also be inspected between turns.
func (s *Service) InspectWorkspace(ctx context.Context, owner, id string, inspect func(context.Context, sandbox.Workspace) error) error {
	live := s.controls.borrowWorkspace(owner, id)
	if live == nil {
		return s.WithWorkspace(ctx, owner, id, inspect)
	}
	defer live.readers.Done()
	ctx, cancel := context.WithTimeout(ctx, workspaceTimeout)
	defer cancel()
	stop := context.AfterFunc(live.ctx, cancel)
	defer stop()
	err := inspect(ctx, live.workspace)
	if errors.Is(err, context.Canceled) && live.ctx.Err() != nil {
		return ErrWorkspaceBusy
	}
	return err
}

// WithWorkspace shares the lease gate with agent execution. It reopens the
// retained workspace for an account's session or project.
func (s *Service) WithWorkspace(ctx context.Context, owner, id string, use func(context.Context, sandbox.Workspace) error) (err error) {
	if !s.controls.leaseMu.TryLock() {
		return ErrWorkspaceBusy
	}
	defer s.controls.leaseMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, workspaceTimeout)
	defer cancel()
	open := timing.Start(ctx, "workspace_acquire")
	ws, err := s.openWorkspace(ctx, owner, id)
	open()
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

func (s *Service) openWorkspace(ctx context.Context, owner, id string) (sandbox.Workspace, error) {
	spec, target, err := sessionSandbox(ctx, s.repo, s.resolve, owner, id, "", false)
	if errors.Is(err, ErrSession) {
		return nil, ErrWorkspaceUnavailable
	}
	if err != nil {
		return nil, err
	}
	spec.RunID = s.newID()
	if project, ok := target.Provider.(sandbox.ProjectProvider); ok {
		ws, err := project.OpenProject(ctx, spec)
		if errors.Is(err, sandbox.ErrWorkspaceMissing) {
			return nil, ErrWorkspaceUnavailable
		}
		return ws, err
	}
	turn, err := s.repo.LatestWorkspaceRun(ctx, owner, id)
	if err != nil {
		return nil, err
	}
	if turn == nil || turn.Backend == "" || turn.Policy != sandbox.Shared {
		return nil, ErrWorkspaceUnavailable
	}
	target, err = s.resolve(ctx, owner, turn.Backend)
	if err != nil {
		return nil, err
	}
	retained, ok := target.Provider.(sandbox.RetainedProvider)
	if !ok {
		return nil, ErrWorkspaceUnavailable
	}
	return retained.OpenRetained(ctx, spec)
}
