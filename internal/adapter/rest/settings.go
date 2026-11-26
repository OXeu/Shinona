package rest

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/settings"
)

func (r *handler) ApplicationSettings(ctx context.Context) []settings.Field {
	if !auth.CurrentUser(ctx).Admin {
		return []settings.Field{}
	}
	return r.app.Fields()
}

type settingChange struct {
	Key   string
	Value *string
}

func settingChanges(input []settingChange) []settings.Change {
	changes := make([]settings.Change, len(input))
	for i, change := range input {
		changes[i] = settings.Change{Key: settings.Key(change.Key), Value: change.Value}
	}
	return changes
}

func (r *handler) UpdateApplicationSettings(ctx context.Context, args struct{ Changes []settingChange }) ([]settings.Field, error) {
	if !auth.CurrentUser(ctx).Admin {
		return nil, codedError{errors.New("administrator access is required"), "ADMIN_REQUIRED"}
	}
	fields, err := r.app.Update(ctx, settingChanges(args.Changes))
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		return nil, codedError{err, invalid.Code}
	}
	if err != nil {
		r.logger.Error("save application settings", "error", err)
		return nil, errors.New("could not save application settings")
	}
	return fields, nil
}

type settingsOverview struct {
	Application []settings.Field         `json:"applicationSettings"`
	Model       []settings.Field         `json:"modelSettings"`
	MCP         settings.MCPSettings     `json:"mcpSettings"`
	SSH         []settings.SSHServerInfo `json:"sshServers"`
}

func (r *handler) settingsOverview(ctx context.Context) (settingsOverview, error) {
	model, err := r.ModelSettings(ctx)
	if err != nil {
		return settingsOverview{}, err
	}
	mcp, err := r.MCPSettings(ctx)
	if err != nil {
		return settingsOverview{}, err
	}
	ssh, err := r.SSHServers(ctx)
	if err != nil {
		return settingsOverview{}, err
	}
	return settingsOverview{Application: r.ApplicationSettings(ctx), Model: model, MCP: mcp, SSH: ssh}, nil
}
