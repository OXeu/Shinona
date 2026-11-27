package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/OXeu/shinona/internal/adapter/httpclient"
	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/mcp"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/settings"
)

type mcpRuntime struct {
	repo   settings.MCPRepository
	oauth  *mcp.OAuth
	client *http.Client
}

func (r *mcpRuntime) tools(ctx context.Context, owner string) (*mcp.Servers, error) {
	servers, err := r.accountMCP(ctx, owner)
	if err != nil {
		return nil, err
	}
	return mcp.NewServers(servers, r.client, r.oauth), nil
}

func (r *mcpRuntime) accountMCP(ctx context.Context, owner string) (settings.MCPConfig, error) {
	if owner == "" {
		return nil, errors.New("MCP settings require an authenticated account")
	}
	return r.repo.MCPConfig(ctx, owner)
}

func (r *mcpRuntime) lookupMCP(ctx context.Context, owner, name string) (settings.MCPServer, bool, error) {
	servers, err := r.accountMCP(ctx, owner)
	if err != nil {
		return settings.MCPServer{}, false, fmt.Errorf("load MCP server settings: %w", err)
	}
	for _, server := range servers {
		if server.Name == name {
			// User OAuth discovery must not opt into loopback network access.
			return server, httpclient.CheckEndpoint(server.URL) == nil, nil
		}
	}
	return settings.MCPServer{}, false, nil
}

func (r *mcpRuntime) registerOAuth(routes httpserver.Routes, requireUser func(http.Handler) http.Handler, secure bool, logger *slog.Logger) {
	r.oauth.Routes(routes, requireUser, mcpOAuthJSON, func(ctx context.Context, owner, name string) (settings.MCPServer, bool) {
		server, found, err := r.lookupMCP(ctx, owner, name)
		if err != nil {
			logger.ErrorContext(ctx, "look up MCP OAuth server", "error", err)
			if response, ok := ctx.Value(mcpOAuthResponseKey{}).(*mcpOAuthResponse); ok {
				response.lookupFailed = true
			}
		}
		return server, found
	}, secure, logger)
}

type mcpOAuthResponseKey struct{}

// The OAuth lookup callback returns a boolean. Preserve lookup failures at
// the HTTP boundary so the JSON routes can report them as server errors.
type mcpOAuthResponse struct {
	http.ResponseWriter
	lookupFailed bool
	wroteFailure bool
}

func mcpOAuthJSON(next http.Handler) http.Handler {
	return sameOriginJSON(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response := &mcpOAuthResponse{ResponseWriter: w}
		ctx := context.WithValue(r.Context(), mcpOAuthResponseKey{}, response)
		next.ServeHTTP(response, r.WithContext(ctx))
	}))
}

func (w *mcpOAuthResponse) WriteHeader(status int) {
	if !w.lookupFailed {
		w.ResponseWriter.WriteHeader(status)
		return
	}
	if !w.wroteFailure {
		w.wroteFailure = true
		httpserver.WriteError(w.ResponseWriter, http.StatusInternalServerError, "SERVICE_UNAVAILABLE", "Could not load MCP server settings.")
	}
}

func (w *mcpOAuthResponse) Write(body []byte) (int, error) {
	if w.lookupFailed {
		w.WriteHeader(http.StatusInternalServerError)
		return len(body), nil
	}
	return w.ResponseWriter.Write(body)
}

func (r *mcpRuntime) MCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	owner := auth.CurrentUser(ctx).ID
	servers, err := r.accountMCP(ctx, owner)
	if err != nil {
		return settings.MCPSettings{}, err
	}
	return r.mcpSettings(ctx, owner, servers)
}

func (r *mcpRuntime) mcpSettings(ctx context.Context, owner string, servers settings.MCPConfig) (settings.MCPSettings, error) {
	connected, err := r.oauth.ConnectedServers(ctx, owner, servers)
	if err != nil {
		return settings.MCPSettings{}, err
	}
	result := servers.Info(len(servers) > 0)
	for i := range result.Servers {
		result.Servers[i].Connected = connected[result.Servers[i].Name]
	}
	return result, nil
}

func (r *mcpRuntime) SaveMCPServer(ctx context.Context, input settings.MCPServerInput) (settings.MCPSettings, error) {
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

func (r *mcpRuntime) RemoveMCPServer(ctx context.Context, name string) (settings.MCPSettings, error) {
	return r.updateMCP(ctx, func(servers settings.MCPConfig) (settings.MCPConfig, error) {
		return servers.Remove(name)
	})
}

func (r *mcpRuntime) ResetMCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	return r.updateMCP(ctx, func(settings.MCPConfig) (settings.MCPConfig, error) {
		return settings.MCPConfig{}, nil
	})
}

func (r *mcpRuntime) updateMCP(ctx context.Context, edit func(settings.MCPConfig) (settings.MCPConfig, error)) (settings.MCPSettings, error) {
	owner := auth.CurrentUser(ctx).ID
	var result settings.MCPSettings
	err := r.oauth.ApplyServerChanges(ctx, owner, func() ([]string, error) {
		var invalidated []string
		var err error
		result, invalidated, err = r.modifyMCP(ctx, owner, edit)
		return invalidated, err
	})
	if err != nil {
		return settings.MCPSettings{}, err
	}
	return result, nil
}

func (r *mcpRuntime) modifyMCP(ctx context.Context, owner string, edit func(settings.MCPConfig) (settings.MCPConfig, error)) (settings.MCPSettings, []string, error) {
	previous, err := r.accountMCP(ctx, owner)
	if err != nil {
		return settings.MCPSettings{}, nil, err
	}
	servers, err := edit(previous)
	if err != nil {
		return settings.MCPSettings{}, nil, err
	}
	// Read grants before saving so a lookup failure can abort the edit.
	result, err := r.mcpSettings(ctx, owner, servers)
	if err != nil {
		return settings.MCPSettings{}, nil, err
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
	return result, invalidated, r.repo.SaveMCPConfig(ctx, owner, servers, invalidated)
}
