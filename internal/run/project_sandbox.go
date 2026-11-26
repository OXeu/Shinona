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
	ErrProjectSandboxConfig = errors.New("invalid project sandbox settings")
	ErrSandboxProvider      = errors.New("the sandbox provider is unavailable")
	templateName            = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._/@:-]{0,254}$`)
)

// ProjectSandboxTimeout bounds provider lifecycle requests, including image pulls.
const ProjectSandboxTimeout = 2 * time.Minute

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

func sessionSandbox(ctx context.Context, repo Repository, resolve ResolveTarget, owner, session, backend string, provision bool) (sandbox.Spec, Target, error) {
	project, err := repo.SessionProject(ctx, owner, session)
	if err != nil {
		return sandbox.Spec{}, Target{}, err
	}
	config, err := repo.ProjectSandboxConfig(ctx, owner, project)
	if err != nil {
		return sandbox.Spec{}, Target{}, err
	}
	spec := sandbox.Spec{OwnerID: owner, SessionID: session, ProjectID: project, Policy: sandbox.Shared}
	if config != nil {
		target, err := resolve(ctx, owner, config.Provider)
		return config.spec(spec), target, err
	}
	target, err := resolve(ctx, owner, backend)
	if err != nil {
		return sandbox.Spec{}, Target{}, err
	}
	if _, managed := target.Provider.(sandbox.ManagedProjectProvider); managed && provision {
		if err := repo.RememberProjectSandboxProvider(ctx, owner, project, target.Name); err != nil {
			return sandbox.Spec{}, Target{}, err
		}
		target, err = resolve(ctx, owner, target.Name)
	}
	return spec, target, err
}

func (c ProjectSandboxConfig) spec(spec sandbox.Spec) sandbox.Spec {
	spec.Template = c.Template
	spec.Timeout = time.Duration(c.TimeoutSeconds) * time.Second
	spec.OnTimeout = c.OnTimeout
	return spec
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
		check, cancel := context.WithTimeout(ctx, workspaceTimeout)
		defer cancel()
		status, err := provider.ProjectStatus(check, sandbox.Spec{OwnerID: owner, ProjectID: id})
		if err == nil {
			result.Status = status
		} else {
			slog.Warn("check project sandbox status", "provider", target.Name, "error", err)
			result.Status.ID = status.ID
		}
	}
	return result, nil
}

func (s *Service) SaveProjectSandbox(ctx context.Context, owner, id string, next ProjectSandboxConfig) error {
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
		if _, ok := target.Provider.(sandbox.ManagedProjectProvider); !ok {
			return ErrSandboxProvider
		}
		if target.Name != "e2b" && (next.TimeoutSeconds != 0 || next.OnTimeout != "") {
			return ErrProjectSandboxConfig
		}
		next.Provider = target.Name
		if current == next {
			return nil
		}
		if current.Provider != "" {
			previous, err := s.resolve(ctx, owner, current.Provider)
			if err != nil {
				return err
			}
			if current.effective(previous) != next.effective(target) {
				provider, _ := previous.Provider.(sandbox.ManagedProjectProvider)
				if err := s.destroyProjectSandbox(ctx, provider, spec); err != nil {
					return err
				}
			}
		}
		save, cancel := context.WithTimeout(context.WithoutCancel(ctx), cleanupTimeout)
		defer cancel()
		return s.repo.SaveProjectSandboxConfig(save, owner, id, next)
	})
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
		if err := s.repo.RememberProjectSandboxProvider(ctx, spec.OwnerID, spec.ProjectID, name); err != nil {
			return err
		}
		target, err := s.resolve(ctx, spec.OwnerID, name)
		if err != nil {
			return err
		}
		var ok bool
		if provider, ok = target.Provider.(sandbox.ManagedProjectProvider); !ok {
			return ErrSandboxProvider
		}
	}
	ws, err := provider.Acquire(ctx, config.spec(spec))
	if err != nil {
		return err
	}
	closeTerminalResource(ctx, ws.Close)
	return nil
}

func (s *Service) projectSandboxConfig(ctx context.Context, owner, id string) (Project, ProjectSandboxConfig, error) {
	project := Project{OwnerID: owner}
	if id != "" {
		var err error
		project, err = s.repo.Project(ctx, owner, id)
		if err != nil {
			return Project{}, ProjectSandboxConfig{}, err
		}
	}
	config, err := s.repo.ProjectSandboxConfig(ctx, owner, id)
	if config == nil || err != nil {
		return project, ProjectSandboxConfig{}, err
	}
	return project, *config, nil
}

func (s *Service) changeProjectSandbox(ctx context.Context, owner, id string, change func(context.Context, ProjectSandboxConfig, sandbox.Spec) error) error {
	if !s.controls.leaseMu.TryLock() {
		return ErrWorkspaceBusy
	}
	defer s.controls.leaseMu.Unlock()
	if !s.projectMu.TryLock() {
		return ErrWorkspaceBusy
	}
	defer s.projectMu.Unlock()
	ctx, cancel := context.WithTimeout(ctx, ProjectSandboxTimeout)
	defer cancel()
	_, config, err := s.projectSandboxConfig(ctx, owner, id)
	if err != nil {
		return err
	}
	spec := sandbox.Spec{RunID: s.newID(), OwnerID: owner, ProjectID: id, Policy: sandbox.Shared}
	return change(ctx, config, spec)
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
