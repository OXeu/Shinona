package run

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
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
	turn, err := s.repo.LatestWorkspaceRun(ctx, owner, id)
	if err != nil {
		return err
	}
	ws, err := s.openWorkspace(ctx, owner, id, turn)
	if err != nil {
		return err
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer stop()
		closeErr := ws.Close(cleanup)
		// Keep an acknowledged operation successful even if lease cleanup fails.
		if err != nil {
			err = errors.Join(err, closeErr)
		}
	}()
	return use(ctx, ws)
}

func (s *Service) openWorkspace(ctx context.Context, owner, id string, turn *Run) (sandbox.Workspace, error) {
	spec := sandbox.Spec{RunID: s.newID(), OwnerID: owner, SessionID: id, Policy: sandbox.Shared, TTL: 30 * time.Second}
	backend := ""
	if turn != nil {
		backend = turn.Backend
	}
	provider := s.resolve(backend).Provider
	if provider == nil {
		provider = s.resolve("").Provider
	}
	if project, ok := provider.(sandbox.ProjectProvider); ok {
		projectID, err := s.repo.SessionProject(ctx, owner, id)
		if errors.Is(err, ErrSession) {
			return nil, ErrWorkspaceUnavailable
		}
		if err != nil {
			return nil, err
		}
		spec.ProjectID = projectID
		ws, err := project.OpenProject(ctx, spec)
		if errors.Is(err, sandbox.ErrWorkspaceMissing) {
			return nil, ErrWorkspaceUnavailable
		}
		return ws, err
	}
	retained, ok := provider.(sandbox.RetainedProvider)
	if !ok || turn == nil || backend == "" || turn.Policy != sandbox.Shared {
		return nil, ErrWorkspaceUnavailable
	}
	return retained.OpenRetained(ctx, spec)
}
