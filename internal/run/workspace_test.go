package run

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestWorkspaceSharesDeadlineAcrossOpenAndUseAndBoundsCleanup(t *testing.T) {
	for _, name := range []string{"inspect", "mutate"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := t.Context()
				start := time.Now()
				limits := WorkspaceTimeouts{Operation: 10 * time.Second, Cleanup: 2 * time.Second}
				box := timedWorkspace{close: func(ctx context.Context) error {
					if now := time.Now(); now.Sub(start) != limits.Operation || ctx.Err() != nil {
						t.Errorf("cleanup started at %v with error %v", now.Sub(start), ctx.Err())
					}
					<-ctx.Done()
					if got := time.Since(start); got != limits.Operation+limits.Cleanup {
						t.Errorf("cleanup finished at %v", got)
					}
					return ctx.Err()
				}}
				provider := timedWorkspaceProvider{open: func(ctx context.Context) (sandbox.Workspace, error) {
					if deadline, ok := ctx.Deadline(); !ok || !deadline.Equal(start.Add(limits.Operation)) {
						t.Errorf("open deadline = %s, %t", deadline, ok)
					}
					time.Sleep(4 * time.Second)
					return box, nil
				}}
				repo := workspaceTestRepository{projects: map[sessionKey]string{{owner: "owner", id: "session"}: ""}}
				resolve := func(context.Context, string, string) (Target, error) { return Target{Provider: provider}, nil }
				service := NewService(ServiceStores{Sessions: repo, Projects: repo}, resolve, func() string { return "lease" }, &Controls{}, nil, limits)
				open := service.InspectWorkspace
				if name == "mutate" {
					open = service.WithWorkspace
				}
				err := open(ctx, "owner", "session", func(ctx context.Context, _ sandbox.Workspace) error {
					<-ctx.Done()
					if got := time.Since(start); got != limits.Operation {
						t.Errorf("workspace use finished at %v", got)
					}
					return ctx.Err()
				})
				if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != limits.Operation+limits.Cleanup {
					t.Fatalf("operation finished after %v with %v", time.Since(start), err)
				}
			})
		})
	}
}

func TestProjectSandboxUsesConfiguredLifecycleTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		limits := WorkspaceTimeouts{Operation: time.Second, Lifecycle: 3 * time.Second}
		repo := workspaceTestRepository{}
		start := time.Now()
		provider := timedWorkspaceProvider{destroy: func(ctx context.Context) error {
			<-ctx.Done()
			return ctx.Err()
		}}
		resolve := func(context.Context, string, string) (Target, error) { return Target{Provider: provider}, nil }
		service := NewService(ServiceStores{Projects: repo}, resolve, func() string { return "lease" }, &Controls{}, nil, limits)
		if err := service.ResetProjectSandbox(t.Context(), "owner", "", false); !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != limits.Lifecycle {
			t.Fatalf("project reset finished after %v with %v", time.Since(start), err)
		}
	})
}

func TestProjectTargetUsesTheBindingThatWonConcurrentProvisioning(t *testing.T) {
	candidate, retained := &timedWorkspaceProvider{}, &timedWorkspaceProvider{}
	repo := concurrentProjectBinding{saved: ProjectSandboxConfig{Provider: sandbox.ProviderE2B, Template: "retained-template"}}
	resolve := func(_ context.Context, _ string, name string) (Target, error) {
		switch name {
		case sandbox.ProviderSSH:
			return Target{Name: name, Provider: candidate}, nil
		case sandbox.ProviderE2B:
			return Target{Name: name, Provider: retained}, nil
		default:
			t.Fatalf("unexpected provider lookup %q", name)
			return Target{}, nil
		}
	}
	spec := sandbox.Spec{OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared}
	spec, target, err := projectTarget(t.Context(), repo, resolve, spec, sandbox.ProviderSSH, true)
	if err != nil || target.Provider != retained || spec.Template != repo.saved.Template {
		t.Fatalf("resolved workspace %+v with target %+v: %v", spec, target, err)
	}
}

type concurrentProjectBinding struct {
	workspaceTestRepository
	saved ProjectSandboxConfig
}

func (r concurrentProjectBinding) RememberProjectSandboxProvider(context.Context, string, string, string) (ProjectSandboxConfig, error) {
	return r.saved, nil
}

type timedWorkspaceProvider struct {
	sandbox.ManagedProjectProvider
	open    func(context.Context) (sandbox.Workspace, error)
	destroy func(context.Context) error
}

func (p timedWorkspaceProvider) OpenProject(ctx context.Context, _ sandbox.Spec) (sandbox.Workspace, error) {
	return p.open(ctx)
}

func (p timedWorkspaceProvider) DestroyProject(ctx context.Context, _ sandbox.Spec) error {
	return p.destroy(ctx)
}

type timedWorkspace struct {
	sandbox.Workspace
	close func(context.Context) error
}

func (w timedWorkspace) Close(ctx context.Context) error { return w.close(ctx) }

func TestInspectWorkspaceBorrowsOnlyTheSessionsLiveLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	active := &activeTurn{item: Run{OwnerID: "owner", SessionID: "session"}, ctx: ctx}
	controls := &Controls{active: map[sessionKey]*activeTurn{active.key(): active}}
	unlock, ok := controls.leases.TryAcquire(sandbox.ProjectKey{OwnerID: "owner"}, false)
	if !ok {
		t.Fatal("could not reserve workspace")
	}
	defer unlock()
	repo := workspaceTestRepository{projects: map[sessionKey]string{{owner: "owner", id: "session"}: ""}}
	service := NewService(ServiceStores{Sessions: repo, Projects: repo}, nil, nil, controls, nil, DefaultWorkspaceTimeouts())
	unexpectedUse := func(context.Context, sandbox.Workspace) error {
		t.Fatal("inspected an unavailable workspace")
		return nil
	}
	if err := service.InspectWorkspace(ctx, "owner", "session", unexpectedUse); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("inspect during provisioning: %v", err)
	}

	box := &stubWorkspace{}
	detach := controls.publishWorkspace(active, box, nil)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	read := make(chan error, 1)
	go func() {
		read <- service.InspectWorkspace(ctx, "owner", "session", func(ctx context.Context, ws sandbox.Workspace) error {
			defer close(canceled)
			close(entered)
			if ws != box {
				return errors.New("inspection used an unexpected workspace")
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("inspection could not borrow the active workspace")
	}
	for _, other := range []sessionKey{{owner: "other", id: "session"}, {owner: "owner", id: "other"}} {
		if err := service.InspectWorkspace(ctx, other.owner, other.id, unexpectedUse); !errors.Is(err, ErrWorkspaceUnavailable) {
			t.Fatalf("inspect another session: %v", err)
		}
	}
	if err := service.WithWorkspace(ctx, "owner", "session", unexpectedUse); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("mutate during a turn: %v", err)
	}
	go func() {
		detach()
		close(release)
	}()
	select {
	case <-release:
		select {
		case <-canceled:
		default:
			t.Fatal("lease detached while inspection was still using it")
		}
	case <-ctx.Done():
		t.Fatal("lease did not cancel the inspection before detaching")
	}
	if err := <-read; !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("inspect during teardown: %v", err)
	}
}

func TestInspectWorkspaceBorrowsTheSharedProjectLease(t *testing.T) {
	for _, project := range []string{"", "project"} {
		t.Run("project="+project, func(t *testing.T) {
			ctx := t.Context()
			active := &activeTurn{item: Run{OwnerID: "owner", SessionID: "first"}, ctx: ctx}
			controls := &Controls{active: map[sessionKey]*activeTurn{active.key(): active}}
			unlock, ok := controls.leases.TryAcquire(sandbox.ProjectKey{OwnerID: "owner", ProjectID: project}, false)
			if !ok {
				t.Fatal("could not reserve workspace")
			}
			defer unlock()
			repo := workspaceTestRepository{projects: map[sessionKey]string{
				{owner: "owner", id: "second"}: project,
				{owner: "owner", id: "other"}:  "other-project",
				{owner: "other", id: "second"}: project,
			}}
			resolve := func(context.Context, string, string) (Target, error) {
				return Target{Provider: workspaceTestProvider{}}, nil
			}
			service := NewService(ServiceStores{Sessions: repo, Projects: repo}, resolve, func() string { return "test-lease" }, controls, nil, DefaultWorkspaceTimeouts())
			box := &stubWorkspace{}
			defer controls.publishWorkspace(active, box, &project)()
			if err := service.InspectWorkspace(ctx, "owner", "second", func(_ context.Context, ws sandbox.Workspace) error {
				if ws != box {
					t.Fatal("inspection did not reuse the project's live workspace")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			for _, other := range []struct {
				key  sessionKey
				want error
			}{
				{sessionKey{"owner", "other"}, nil},
				{sessionKey{"other", "second"}, nil},
				{sessionKey{"owner", "missing"}, ErrWorkspaceUnavailable},
			} {
				err := service.InspectWorkspace(ctx, other.key.owner, other.key.id, func(_ context.Context, ws sandbox.Workspace) error {
					if ws == box || other.want != nil {
						t.Fatal("inspection borrowed a workspace outside the account and project")
					}
					return nil
				})
				if !errors.Is(err, other.want) {
					t.Fatalf("inspect %+v: got %v, want %v", other.key, err, other.want)
				}
			}
		})
	}
}

func TestInspectWorkspaceAllowsConcurrentRetainedReads(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	repo := workspaceTestRepository{projects: map[sessionKey]string{
		{owner: "owner", id: "first"}:  "project",
		{owner: "owner", id: "second"}: "project",
	}}
	resolve := func(context.Context, string, string) (Target, error) {
		return Target{Provider: workspaceTestProvider{}}, nil
	}
	service := NewService(ServiceStores{Sessions: repo, Projects: repo}, resolve, func() string { return "test-lease" }, &Controls{}, nil, DefaultWorkspaceTimeouts())
	entered := make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- service.InspectWorkspace(ctx, "owner", "first", func(ctx context.Context, _ sandbox.Workspace) error {
			close(entered)
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-entered:
	case err := <-first:
		t.Fatalf("first inspection failed: %v", err)
	case <-ctx.Done():
		t.Fatal("first inspection did not start")
	}
	if err := service.InspectWorkspace(ctx, "owner", "second", func(context.Context, sandbox.Workspace) error {
		return nil
	}); err != nil {
		t.Fatalf("second inspection failed while the first was still running: %v", err)
	}
	if err := service.WithWorkspace(ctx, "owner", "second", func(context.Context, sandbox.Workspace) error {
		t.Fatal("mutation ran while the workspace was being inspected")
		return nil
	}); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("mutate during an inspection: %v", err)
	}
	cancel()
	if err := <-first; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel first inspection: %v", err)
	}
}

type workspaceTestRepository struct {
	SessionRepository
	ProjectRepository
	projects map[sessionKey]string
}

func (r workspaceTestRepository) SessionProject(_ context.Context, owner, id string) (string, error) {
	if project, ok := r.projects[sessionKey{owner, id}]; ok {
		return project, nil
	}
	return "", ErrSession
}

func (workspaceTestRepository) ProjectSandboxConfig(context.Context, string, string) (*ProjectSandboxConfig, error) {
	return nil, nil
}

type workspaceTestProvider struct{ sandbox.Provider }

func (workspaceTestProvider) OpenProject(context.Context, sandbox.Spec) (sandbox.Workspace, error) {
	return &stubWorkspace{}, nil
}

type stubWorkspace struct {
	sandbox.Workspace
}

func (*stubWorkspace) Close(context.Context) error { return nil }
