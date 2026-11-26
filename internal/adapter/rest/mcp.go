package rest

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/settings"
)

func (r *handler) MCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	result, err := r.app.MCPSettings(ctx)
	return result, r.mcpError(err)
}

func (r *handler) SaveMCPServer(ctx context.Context, input settings.MCPServerInput) (settings.MCPSettings, error) {
	result, err := r.app.SaveMCPServer(ctx, input)
	return result, r.mcpError(err)
}

func (r *handler) RemoveMCPServer(ctx context.Context, args struct {
	Name string `form:"name"`
}) (settings.MCPSettings, error) {
	result, err := r.app.RemoveMCPServer(ctx, args.Name)
	return result, r.mcpError(err)
}

func (r *handler) ResetMCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	result, err := r.app.ResetMCPSettings(ctx)
	return result, r.mcpError(err)
}

func (r *handler) mcpError(err error) error {
	if err == nil {
		return nil
	}
	if invalid, ok := errors.AsType[*settings.ValidationError](err); ok {
		return codedError{err, invalid.Code}
	}
	r.logger.Error("save MCP settings", "error", err)
	return errors.New("could not save MCP settings")
}
