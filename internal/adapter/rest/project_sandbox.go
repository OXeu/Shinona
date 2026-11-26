package rest

import (
	"context"
	"net/http"
	"time"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

type projectSandboxConfig struct {
	Provider       string `json:"provider"`
	Template       string `json:"template"`
	TimeoutSeconds int32  `json:"timeoutSeconds"`
	OnTimeout      string `json:"onTimeout"`
}

func sandboxConfig(config run.ProjectSandboxConfig) projectSandboxConfig {
	return projectSandboxConfig{
		Provider: config.Provider, Template: config.Template,
		TimeoutSeconds: int32(config.TimeoutSeconds), OnTimeout: config.OnTimeout,
	}
}

type projectSandboxResponse struct {
	Project       projectResponse          `json:"project"`
	Config        projectSandboxConfig     `json:"config"`
	Effective     projectSandboxConfig     `json:"effective"`
	ProviderLabel string                   `json:"providerLabel"`
	Providers     []projectSandboxProvider `json:"providers"`
	SandboxID     string                   `json:"sandboxId"`
	State         string                   `json:"state"`
}

type projectSandboxProvider struct {
	projectSandboxConfig
	Label string `json:"label"`
}

func (r *handler) ProjectSandbox(ctx context.Context, args resourceID) (projectSandboxResponse, error) {
	setSandboxWriteDeadline(ctx, 20*time.Second)
	status, err := r.service.ProjectSandbox(ctx, auth.CurrentUser(ctx).ID, projectID(args.ID))
	if err != nil {
		return projectSandboxResponse{}, r.apiError(err)
	}
	providers := make([]projectSandboxProvider, len(status.Providers))
	for i, item := range status.Providers {
		config := item.Defaults
		config.Provider = item.Name
		providers[i] = projectSandboxProvider{projectSandboxConfig: sandboxConfig(config), Label: item.Label}
	}
	return projectSandboxResponse{
		Project:   projectResponse{ID: status.Project.ID, Name: status.Project.Name},
		Config:    sandboxConfig(status.Config),
		Effective: sandboxConfig(status.Effective), ProviderLabel: status.ProviderLabel, Providers: providers,
		SandboxID: status.Status.ID, State: string(status.Status.State),
	}, nil
}

func (r *handler) SaveProjectSandbox(ctx context.Context, args struct {
	ID             string `uri:"id" json:"-"`
	Input          projectSandboxConfig
	SkipCleanupFor *string
}) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	var skipCleanupFor string
	if args.SkipCleanupFor != nil {
		skipCleanupFor = *args.SkipCleanupFor
	}
	err := r.service.SaveProjectSandbox(ctx, auth.CurrentUser(ctx).ID, projectID(args.ID), run.ProjectSandboxConfig{
		Provider: args.Input.Provider, Template: args.Input.Template,
		TimeoutSeconds: int(args.Input.TimeoutSeconds), OnTimeout: args.Input.OnTimeout,
	}, skipCleanupFor)
	return err == nil, r.apiError(err)
}

func (r *handler) DestroyProjectSandbox(ctx context.Context, args resourceID) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	err := r.service.ResetProjectSandbox(ctx, auth.CurrentUser(ctx).ID, projectID(args.ID), false)
	return err == nil, r.apiError(err)
}

func (r *handler) CreateProjectSandbox(ctx context.Context, args resourceID) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	err := r.service.CreateProjectSandbox(ctx, auth.CurrentUser(ctx).ID, projectID(args.ID))
	return err == nil, r.apiError(err)
}

func (r *handler) RecreateProjectSandbox(ctx context.Context, args resourceID) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	err := r.service.ResetProjectSandbox(ctx, auth.CurrentUser(ctx).ID, projectID(args.ID), true)
	return err == nil, r.apiError(err)
}

func setSandboxWriteDeadline(ctx context.Context, duration time.Duration) {
	if controller, ok := ctx.Value(responseControllerKey{}).(*http.ResponseController); ok {
		_ = controller.SetWriteDeadline(time.Now().Add(duration))
	}
}

// The default project has an empty ID in the domain model.
func projectID(id string) string {
	if id == "default" {
		return ""
	}
	return id
}
