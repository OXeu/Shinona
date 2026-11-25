package main

import (
	"context"
	"errors"
	"strings"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/settings"
)

func (r *runtime) accountMCP(ctx context.Context, owner string) (settings.MCPConfig, error) {
	if owner == "" {
		return nil, errors.New("MCP settings require an authenticated account")
	}
	return r.repo.MCPConfig(ctx, owner)
}

func (r *runtime) lookupMCP(ctx context.Context, owner, name string) (settings.MCPServer, bool) {
	servers, err := r.accountMCP(ctx, owner)
	if err == nil {
		for _, server := range servers {
			if server.Name == name {
				// User OAuth discovery must not opt into loopback network access.
				return server, httpclient.CheckEndpoint(server.URL) == nil
			}
		}
	}
	return settings.MCPServer{}, false
}

func (r *runtime) MCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	owner := auth.CurrentUser(ctx).ID
	servers, err := r.accountMCP(ctx, owner)
	if err != nil {
		return settings.MCPSettings{}, err
	}
	return r.mcpSettings(ctx, owner, servers), nil
}

func (r *runtime) mcpSettings(ctx context.Context, owner string, servers settings.MCPConfig) settings.MCPSettings {
	result := servers.Info(len(servers) > 0)
	connected := make(map[string]bool)
	for _, server := range servers {
		if server.OAuth != nil {
			connected[server.Name] = r.mcpOAuth.Connected(ctx, owner, server)
		}
	}
	for i := range result.Servers {
		result.Servers[i].Connected = connected[result.Servers[i].Name]
	}
	return result
}

func (r *runtime) SaveMCPServer(ctx context.Context, input settings.MCPServerInput) (settings.MCPSettings, error) {
	return r.updateMCP(ctx, func(servers settings.MCPConfig) (settings.MCPConfig, error) {
		next, err := servers.Save(input)
		if err != nil {
			return nil, err
		}
		if err := httpclient.CheckEndpoint(strings.TrimSpace(input.URL)); err != nil {
			return nil, settings.Invalid("PRIVATE_ENDPOINT", err.Error())
		}
		return next, nil
	})
}

func (r *runtime) RemoveMCPServer(ctx context.Context, name string) (settings.MCPSettings, error) {
	return r.updateMCP(ctx, func(servers settings.MCPConfig) (settings.MCPConfig, error) {
		return servers.Remove(name)
	})
}

func (r *runtime) ResetMCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	return r.updateMCP(ctx, func(settings.MCPConfig) (settings.MCPConfig, error) {
		return settings.MCPConfig{}, nil
	})
}

func (r *runtime) updateMCP(ctx context.Context, edit func(settings.MCPConfig) (settings.MCPConfig, error)) (settings.MCPSettings, error) {
	r.mcpMu.Lock()
	defer r.mcpMu.Unlock()

	owner := auth.CurrentUser(ctx).ID
	previous, err := r.accountMCP(ctx, owner)
	if err != nil {
		return settings.MCPSettings{}, err
	}
	servers, err := edit(previous)
	if err != nil {
		return settings.MCPSettings{}, err
	}

	next := make(map[string]settings.MCPServer, len(servers))
	for _, server := range servers {
		next[server.Name] = server
	}
	var invalidated []string
	for _, server := range previous {
		current := next[server.Name]
		if server.OAuth != nil && (current.URL != server.URL || current.OAuth == nil || *current.OAuth != *server.OAuth) {
			invalidated = append(invalidated, server.Name)
		}
	}
	if err := r.mcpOAuth.ApplyServerChanges(owner, invalidated, func() error {
		return r.repo.SaveMCPConfig(ctx, owner, servers, invalidated)
	}); err != nil {
		return settings.MCPSettings{}, err
	}
	return r.mcpSettings(ctx, owner, servers), nil
}
