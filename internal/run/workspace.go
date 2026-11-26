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

func (c *Controls) publishWorkspace(active *activeTurn, ws sandbox.Workspace, projectID *string) func() {
	ctx, cancel := context.WithCancel(active.ctx)
	lease := &liveWorkspace{workspace: ws, ctx: ctx}
	key := sandbox.ProjectKey{OwnerID: active.item.OwnerID}
	c.mu.Lock()
	active.workspace = lease
	if projectID != nil {
		key.ProjectID = *projectID
		if c.liveProjects == nil {
			c.liveProjects = make(map[sandbox.ProjectKey]*liveWorkspace)
		}
		c.liveProjects[key] = lease
	}
	c.mu.Unlock()

	return func() {
		c.mu.Lock()
		active.workspace = nil
		if projectID != nil {
			delete(c.liveProjects, key)
		}
		c.mu.Unlock()
		cancel()
		// Drain borrowed readers before the worker closes the provider's lease.
		lease.readers.Wait()
	}
}

func (s *Service) borrowWorkspace(ctx context.Context, owner, id string) (*liveWorkspace, error) {
	c := s.controls
	c.mu.Lock()
	active := c.active[sessionKey{owner, id}]
	if active != nil && active.workspace != nil && active.workspace.ctx.Err() == nil {
		live := active.workspace
		live.readers.Add(1)
		c.mu.Unlock()
		return live, nil
	}
	shared := len(c.liveProjects) != 0
	c.mu.Unlock()
	if !shared {
		return nil, nil
	}
	projectID, err := s.repo.SessionProject(ctx, owner, id)
	if errors.Is(err, ErrSession) {
		return nil, ErrWorkspaceUnavailable
	}
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	live := c.liveProjects[sandbox.ProjectKey{OwnerID: owner, ProjectID: projectID}]
	if live == nil || live.ctx.Err() != nil {
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

// A project deletion can move a session to the default project while a
// request waits for its gate. Verify membership after acquiring the gate.
func lockSessionProject(ctx context.Context, repo Repository, owner, id string, acquire func(sandbox.ProjectKey) (func(), error)) (sandbox.Spec, func(), error) {
	for {
		project, err := repo.SessionProject(ctx, owner, id)
		if err != nil {
			return sandbox.Spec{}, nil, err
		}
		spec := sandbox.Spec{OwnerID: owner, SessionID: id, ProjectID: project, Policy: sandbox.Shared}
		release, err := acquire(spec.ProjectKey())
		if err != nil {
			return sandbox.Spec{}, nil, err
		}
		current, err := repo.SessionProject(ctx, owner, id)
		if err == nil && current == project {
			return spec, release, nil
		}
		release()
		if err != nil {
			return sandbox.Spec{}, nil, err
		}
	}
}

func (s *Service) withWorkspace(ctx context.Context, owner, id string, readOnly bool, use func(context.Context, sandbox.Workspace) error) (err error) {
	ctx, cancel := context.WithTimeout(ctx, workspaceTimeout)
	defer cancel()
	acquired := sync.OnceFunc(timing.Start(ctx, "workspace_acquire"))
	defer acquired()
	resolve := sync.OnceFunc(timing.Start(ctx, "workspace_resolve"))
	defer resolve()
	spec, release, err := lockSessionProject(ctx, s.repo, owner, id, func(key sandbox.ProjectKey) (func(), error) {
		if release, ok := s.controls.leases.TryAcquire(key, readOnly); ok {
			return release, nil
		}
		return nil, ErrWorkspaceBusy
	})
	if errors.Is(err, ErrSession) {
		return ErrWorkspaceUnavailable
	}
	if err != nil {
		return err
	}
	defer release()
	spec, target, err := projectTarget(ctx, s.repo, s.resolve, spec, "", false)
	resolve()
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
	ws, err := openWorkspace(ctx, spec, target)
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

func openWorkspace(ctx context.Context, spec sandbox.Spec, target Target) (sandbox.Workspace, error) {
	if project, ok := target.Provider.(sandbox.ProjectProvider); ok {
		ws, err := project.OpenProject(ctx, spec)
		return ws, workspaceAvailability(err)
	}
	return nil, ErrWorkspaceUnavailable
}
