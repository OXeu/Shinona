package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/OXeu/shinona/internal/adapter/e2b"
	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/adapter/mcp"
	"github.com/OXeu/shinona/internal/settings"
)

type runtimeRepository interface {
	settings.Repository
	settings.ModelRepository
	settings.MCPRepository
	settings.SSHRepository
	mcp.OAuthStore
}

type runtime struct {
	*instanceSettings
	*modelRuntime
	*mcpRuntime
	*sshRuntime
}

func newRuntime(ctx context.Context, repo runtimeRepository, workspaces e2b.WorkspaceStore, environment environment) (*runtime, error) {
	origin, err := webOrigin(environment.value("SHINONA_WEB_ORIGIN", "http://127.0.0.1:5173"))
	if err != nil {
		return nil, err
	}
	modelPolicy, err := httpclient.ParseModelPolicy(environment.value("SHINONA_TRUSTED_MODEL_ORIGINS", ""))
	if err != nil {
		return nil, fmt.Errorf("SHINONA_TRUSTED_MODEL_ORIGINS: %w", err)
	}
	sshSettings, err := newSSHRuntime(repo, environment)
	if err != nil {
		return nil, err
	}
	publicHTTP := httpclient.Public()
	mcpSettings := &mcpRuntime{repo: repo, oauth: mcp.NewOAuth(repo, origin.String()), client: publicHTTP}
	models := newModelRuntime(repo, modelPolicy, mcpSettings)
	instance := &instanceSettings{
		repo: repo, defaults: settings.Defaults(environment.lookup), origin: origin, publicHTTP: publicHTTP,
		e2bStore: workspaces, buildSSH: sshSettings.sshSandboxProvider, agent: models.resolveAgent,
		e2bHTTP: &http.Client{
			Timeout:       35 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	if err := instance.initialize(ctx); err != nil {
		return nil, err
	}
	return &runtime{instanceSettings: instance, modelRuntime: models, mcpRuntime: mcpSettings, sshRuntime: sshSettings}, nil
}
