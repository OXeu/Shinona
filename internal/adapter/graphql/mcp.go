package graphql

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/settings"
)

func (r *resolver) MCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	result, err := r.app.MCPSettings(ctx)
	return result, r.mcpError(err)
}

func (r *resolver) SaveMCPServer(ctx context.Context, args struct{ Input settings.MCPServerInput }) (settings.MCPSettings, error) {
	result, err := r.app.SaveMCPServer(ctx, args.Input)
	return result, r.mcpError(err)
}

func (r *resolver) RemoveMCPServer(ctx context.Context, args struct{ Name string }) (settings.MCPSettings, error) {
	result, err := r.app.RemoveMCPServer(ctx, args.Name)
	return result, r.mcpError(err)
}

func (r *resolver) ResetMCPSettings(ctx context.Context) (settings.MCPSettings, error) {
	result, err := r.app.ResetMCPSettings(ctx)
	return result, r.mcpError(err)
}

func (r *resolver) mcpError(err error) error {
	if err == nil {
		return nil
	}
	if invalid, ok := errors.AsType[*settings.ValidationError](err); ok {
		return codedError{err, invalid.Code}
	}
	r.logger.Error("save MCP settings", "error", err)
	return errors.New("could not save MCP settings")
}
