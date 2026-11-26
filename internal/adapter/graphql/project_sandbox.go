package graphql

import (
	"context"
	"net/http"
	"time"

	gql "github.com/graph-gophers/graphql-go"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

type projectSandboxConfig struct {
	Provider       string
	Template       string
	TimeoutSeconds int32
	OnTimeout      string
}

func sandboxConfig(config run.ProjectSandboxConfig) projectSandboxConfig {
	return projectSandboxConfig{
		Provider: config.Provider, Template: config.Template,
		TimeoutSeconds: int32(config.TimeoutSeconds), OnTimeout: config.OnTimeout,
	}
}

type projectSandboxResolver struct {
	Project       projectResolver
	Config        projectSandboxConfig
	Effective     projectSandboxConfig
	ProviderLabel string
	Providers     []projectSandboxProvider
	SandboxID     string
	State         string
}

type projectSandboxProvider struct {
	projectSandboxConfig
	Label string
}

func (r *resolver) ProjectSandbox(ctx context.Context, args struct{ ProjectID gql.ID }) (projectSandboxResolver, error) {
	setSandboxWriteDeadline(ctx, 20*time.Second)
	status, err := r.service.ProjectSandbox(ctx, auth.CurrentUser(ctx).ID, string(args.ProjectID))
	if err != nil {
		return projectSandboxResolver{}, r.apiError(err)
	}
	providers := make([]projectSandboxProvider, len(status.Providers))
	for i, item := range status.Providers {
		config := item.Defaults
		config.Provider = item.Name
		providers[i] = projectSandboxProvider{projectSandboxConfig: sandboxConfig(config), Label: item.Label}
	}
	return projectSandboxResolver{
		Project:   projectResolver{ID: gql.ID(status.Project.ID), Name: status.Project.Name},
		Config:    sandboxConfig(status.Config),
		Effective: sandboxConfig(status.Effective), ProviderLabel: status.ProviderLabel, Providers: providers,
		SandboxID: status.Status.ID, State: string(status.Status.State),
	}, nil
}

func (r *resolver) SaveProjectSandbox(ctx context.Context, args struct {
	ProjectID      gql.ID
	Input          projectSandboxConfig
	SkipCleanupFor *string
}) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	var skipCleanupFor string
	if args.SkipCleanupFor != nil {
		skipCleanupFor = *args.SkipCleanupFor
	}
	err := r.service.SaveProjectSandbox(ctx, auth.CurrentUser(ctx).ID, string(args.ProjectID), run.ProjectSandboxConfig{
		Provider: args.Input.Provider, Template: args.Input.Template,
		TimeoutSeconds: int(args.Input.TimeoutSeconds), OnTimeout: args.Input.OnTimeout,
	}, skipCleanupFor)
	return err == nil, r.apiError(err)
}

func (r *resolver) DestroyProjectSandbox(ctx context.Context, args struct{ ProjectID gql.ID }) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	err := r.service.ResetProjectSandbox(ctx, auth.CurrentUser(ctx).ID, string(args.ProjectID), false)
	return err == nil, r.apiError(err)
}

func (r *resolver) CreateProjectSandbox(ctx context.Context, args struct{ ProjectID gql.ID }) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	err := r.service.CreateProjectSandbox(ctx, auth.CurrentUser(ctx).ID, string(args.ProjectID))
	return err == nil, r.apiError(err)
}

func (r *resolver) RecreateProjectSandbox(ctx context.Context, args struct{ ProjectID gql.ID }) (bool, error) {
	setSandboxWriteDeadline(ctx, run.ProjectSandboxTimeout+30*time.Second)
	err := r.service.ResetProjectSandbox(ctx, auth.CurrentUser(ctx).ID, string(args.ProjectID), true)
	return err == nil, r.apiError(err)
}

func setSandboxWriteDeadline(ctx context.Context, duration time.Duration) {
	if controller, ok := ctx.Value(responseControllerKey{}).(*http.ResponseController); ok {
		_ = controller.SetWriteDeadline(time.Now().Add(duration))
	}
}
