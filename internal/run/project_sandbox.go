package run

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

var (
	ErrProjectSandboxConfig  = errors.New("invalid project sandbox settings")
	ErrProjectSandboxCleanup = errors.New("could not clean up the previous project sandbox")
	ErrSandboxProvider       = errors.New("the sandbox provider is unavailable")
	templateName             = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/@:-]{0,254}$`)
)

type ProjectSandboxConfig struct {
	Provider       string
	Template       string
	TimeoutSeconds int
	OnTimeout      string
}

type ProjectSandbox struct {
	Project       Project
	Config        ProjectSandboxConfig
	Effective     ProjectSandboxConfig
	ProviderLabel string
	Providers     []Target
	Status        sandbox.ProjectStatus
}

func projectTarget(ctx context.Context, repo ProjectSandboxRepository, resolve ResolveTarget, spec sandbox.Spec, backend string, provision bool) (sandbox.Spec, Target, error) {
	config, err := repo.ProjectSandboxConfig(ctx, spec.OwnerID, spec.ProjectID)
	if err != nil {
		return sandbox.Spec{}, Target{}, err
	}
	if config == nil {
		target, err := resolve(ctx, spec.OwnerID, backend)
		if err != nil {
			return sandbox.Spec{}, Target{}, err
		}
		if _, managed := target.Provider.(sandbox.ManagedProjectProvider); !managed || !provision {
			return spec, target, nil
		}
		saved, err := repo.RememberProjectSandboxProvider(ctx, spec.OwnerID, spec.ProjectID, target.Name)
		if err != nil {
			return sandbox.Spec{}, Target{}, err
		}
		config = &saved
	}
	target, err := resolve(ctx, spec.OwnerID, config.Provider)
	if err != nil {
		return sandbox.Spec{}, Target{}, err
	}
	spec.Template = config.Template
	target.Provider, err = config.configureIdleTimeout(target.Provider)
	return spec, target, err
}

func (c ProjectSandboxConfig) configureIdleTimeout(provider sandbox.Provider) (sandbox.Provider, error) {
	if c.TimeoutSeconds == 0 && c.OnTimeout == "" {
		return provider, nil
	}
	managed, ok := provider.(sandbox.IdleTimeoutProvider)
	if !ok {
		return nil, ErrProjectSandboxConfig
	}
	return managed.WithIdleTimeout(sandbox.IdleTimeout{After: time.Duration(c.TimeoutSeconds) * time.Second, Action: c.OnTimeout}), nil
}

func (c ProjectSandboxConfig) effective(target Target) ProjectSandboxConfig {
	c.Provider = target.Name
	if c.Template == "" {
		c.Template = target.Defaults.Template
	}
	if c.TimeoutSeconds == 0 {
		c.TimeoutSeconds = target.Defaults.TimeoutSeconds
	}
	if c.OnTimeout == "" {
		c.OnTimeout = target.Defaults.OnTimeout
	}
	return c
}

func (s *Service) ProjectSandbox(ctx context.Context, owner, id string) (ProjectSandbox, error) {
	ctx, cancel := context.WithTimeout(ctx, s.workspace.Operation)
	defer cancel()
	project, config, err := s.projectSandboxConfig(ctx, owner, id)
	if err != nil {
		return ProjectSandbox{}, err
	}
	target, err := s.resolve(ctx, owner, config.Provider)
	if err != nil {
		return ProjectSandbox{}, err
	}
	providers, err := s.projectTargets(ctx, owner)
	if err != nil {
		return ProjectSandbox{}, err
	}
	result := ProjectSandbox{
		Project: project, Config: config, Effective: config.effective(target),
		ProviderLabel: target.Label, Providers: providers, Status: sandbox.ProjectStatus{State: sandbox.ProjectUnavailable},
	}
	if provider, ok := target.Provider.(sandbox.ManagedProjectProvider); ok {
		if config.Provider == "" {
			result.Status.State = sandbox.ProjectNone
			return result, nil
		}
		status, err := provider.ProjectStatus(ctx, sandbox.Spec{OwnerID: owner, ProjectID: id})
		if err == nil {
			result.Status = status
		} else {
			slog.Warn("check project sandbox status", "provider", target.Name, "error", err)
			result.Status.ID = status.ID
		}
	}
	return result, nil
}

func (s *Service) SaveProjectSandbox(ctx context.Context, owner, id string, next ProjectSandboxConfig, skipCleanupFor string) error {
	next.Template = strings.TrimSpace(next.Template)
	if len(next.Provider) > 64 || next.Template != "" && !templateName.MatchString(next.Template) ||
		next.TimeoutSeconds < 0 || next.TimeoutSeconds > sandbox.MaxTimeoutSeconds ||
		next.OnTimeout != "" && next.OnTimeout != sandbox.TimeoutPause && next.OnTimeout != sandbox.TimeoutKill {
		return ErrProjectSandboxConfig
	}
	return s.changeProjectSandbox(ctx, owner, id, func(ctx context.Context, current ProjectSandboxConfig, spec sandbox.Spec) error {
		target, err := s.resolve(ctx, owner, next.Provider)
		if err != nil {
			return err
		}
		provider, ok := target.Provider.(sandbox.ManagedProjectProvider)
		if !ok {
			return ErrSandboxProvider
		}
		if next.TimeoutSeconds != 0 || next.OnTimeout != "" {
			if _, ok := provider.(sandbox.IdleTimeoutProvider); !ok {
				return ErrProjectSandboxConfig
			}
		}
		next.Provider = target.Name
		if current == next {
			return nil
		}
		if skipCleanupFor != "" && (current.Provider != skipCleanupFor || current.Provider == next.Provider) {
			return ErrProjectSandboxConfig
		}
		switch {
		case current.Provider == "":
		case current.Provider != next.Provider:
			if err := s.terminals.stopProject(ctx, owner, id); err != nil {
				return err
			}
			if skipCleanupFor == "" {
				if err := s.cleanPreviousSandbox(ctx, current.Provider, spec); err != nil {
					slog.Warn("clean up project sandbox before provider change", "provider", current.Provider, "error", err)
					return ErrProjectSandboxCleanup
				}
			}
		case current.effective(target) != next.effective(target):
			if err := s.destroyProjectSandbox(ctx, provider, spec); err != nil {
				return err
			}
		}
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), completionWriteTimeout)
		defer cancel()
		return s.stores.Projects.SaveProjectSandboxConfig(save, owner, id, next)
	})
}

func (s *Service) cleanPreviousSandbox(ctx context.Context, name string, spec sandbox.Spec) error {
	ctx, cancel := context.WithTimeout(ctx, s.workspace.Operation)
	defer cancel()
	previous, err := s.resolve(ctx, spec.OwnerID, name)
	if err != nil {
		return err
	}
	if provider, ok := previous.Provider.(sandbox.ManagedProjectProvider); ok {
		return provider.DestroyProject(ctx, spec)
	}
	return ErrSandboxProvider
}

func (s *Service) ResetProjectSandbox(ctx context.Context, owner, id string, recreate bool) error {
	return s.changeProjectSandbox(ctx, owner, id, func(ctx context.Context, config ProjectSandboxConfig, spec sandbox.Spec) error {
		target, err := s.resolve(ctx, owner, config.Provider)
		if err != nil {
			return err
		}
		provider, ok := target.Provider.(sandbox.ManagedProjectProvider)
		if !ok {
			return ErrSandboxProvider
		}
		if err := s.destroyProjectSandbox(ctx, provider, spec); err != nil || !recreate {
			return err
		}
		return s.provisionProjectSandbox(ctx, target.Name, provider, config, spec)
	})
}

func (s *Service) CreateProjectSandbox(ctx context.Context, owner, id string) error {
	return s.changeProjectSandbox(ctx, owner, id, func(ctx context.Context, config ProjectSandboxConfig, spec sandbox.Spec) error {
		target, err := s.resolve(ctx, owner, config.Provider)
		if err != nil {
			return err
		}
		provider, ok := target.Provider.(sandbox.ManagedProjectProvider)
		if !ok {
			return ErrSandboxProvider
		}
		return s.provisionProjectSandbox(ctx, target.Name, provider, config, spec)
	})
}

func (s *Service) provisionProjectSandbox(ctx context.Context, name string, provider sandbox.ManagedProjectProvider, config ProjectSandboxConfig, spec sandbox.Spec) error {
	if config.Provider == "" {
		saved, err := s.stores.Projects.RememberProjectSandboxProvider(ctx, spec.OwnerID, spec.ProjectID, name)
		if err != nil {
			return err
		}
		config = saved
		target, err := s.resolve(ctx, spec.OwnerID, config.Provider)
		if err != nil {
			return err
		}
		var ok bool
		if provider, ok = target.Provider.(sandbox.ManagedProjectProvider); !ok {
			return ErrSandboxProvider
		}
	}
	configured, err := config.configureIdleTimeout(provider)
	if err != nil {
		return err
	}
	spec.Template = config.Template
	ws, err := configured.Acquire(ctx, spec)
	if err != nil {
		return err
	}
	_ = s.workspace.closeResource(ctx, ws.Close)
	return nil
}

func (s *Service) projectSandboxConfig(ctx context.Context, owner, id string) (Project, ProjectSandboxConfig, error) {
	project := Project{OwnerID: owner}
	if id != "" {
		var err error
		project, err = s.stores.Projects.Project(ctx, owner, id)
		if err != nil {
			return Project{}, ProjectSandboxConfig{}, err
		}
	}
	config, err := s.stores.Projects.ProjectSandboxConfig(ctx, owner, id)
	if config == nil || err != nil {
		return project, ProjectSandboxConfig{}, err
	}
	return project, *config, nil
}

func (s *Service) changeProjectSandbox(ctx context.Context, owner, id string, change func(context.Context, ProjectSandboxConfig, sandbox.Spec) error) error {
	release, ok := s.tryProjectChange(owner, id)
	if !ok {
		return ErrWorkspaceBusy
	}
	defer release()
	ctx, cancel := context.WithTimeout(ctx, s.workspace.Lifecycle)
	defer cancel()
	_, config, err := s.projectSandboxConfig(ctx, owner, id)
	if err != nil {
		return err
	}
	spec := sandbox.Spec{RunID: s.newID(), OwnerID: owner, ProjectID: id, Policy: sandbox.Shared}
	return change(ctx, config, spec)
}

func (s *Service) tryProjectChange(owner, id string) (func(), bool) {
	key := sandbox.ProjectKey{OwnerID: owner, ProjectID: id}
	lease, ok := s.controls.leases.TryAcquire(key, false)
	if !ok {
		return nil, false
	}
	change, ok := s.projectChanges.TryAcquire(key, false)
	if !ok {
		lease()
		return nil, false
	}
	return func() {
		change()
		lease()
	}, true
}

func (s *Service) destroyProjectSandbox(ctx context.Context, provider sandbox.ManagedProjectProvider, spec sandbox.Spec) error {
	if err := s.terminals.stopProject(ctx, spec.OwnerID, spec.ProjectID); err != nil {
		return err
	}
	if provider == nil {
		return nil
	}
	return provider.DestroyProject(ctx, spec)
}
