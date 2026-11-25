package graphql

import (
	"context"
	"errors"

	gql "github.com/graph-gophers/graphql-go"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

type ComposerOptions struct {
	DefaultModel  string
	Models        []ModelInfo
	ModelsLoading bool
	MCPServers    []string
	Defaults      *run.ComposerDefaults
}

type ModelInfo struct {
	ID            string
	ContextWindow int32
}

func (r *resolver) ComposerOptions(ctx context.Context) (ComposerOptions, error) {
	defaults, err := r.service.ComposerDefaults(ctx, auth.CurrentUser(ctx).ID)
	if err != nil {
		return ComposerOptions{}, r.apiError(err)
	}
	options, err := r.app.ComposerOptions(ctx)
	if err != nil {
		return ComposerOptions{}, r.apiError(err)
	}
	options.Defaults = defaults
	return options, nil
}

func (r *resolver) SetComposerDefaults(ctx context.Context, args struct{ Input run.ComposerDefaults }) (run.ComposerDefaults, error) {
	if err := r.service.SetComposerDefaults(ctx, auth.CurrentUser(ctx).ID, args.Input); err != nil {
		return run.ComposerDefaults{}, r.apiError(err)
	}
	return args.Input, nil
}

func (r *resolver) ModelSettings(ctx context.Context) ([]settings.Field, error) {
	fields, err := r.app.ModelFields(ctx)
	if err != nil {
		return nil, r.apiError(err)
	}
	return fields, nil
}

func (r *resolver) UpdateModelSettings(ctx context.Context, args struct{ Changes []settingChange }) ([]settings.Field, error) {
	fields, err := r.app.UpdateModelSettings(ctx, settingChanges(args.Changes))
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		return nil, codedError{err, invalid.Code}
	}
	if err != nil {
		return nil, r.apiError(err)
	}
	return fields, nil
}

func (r *resolver) FetchModels(ctx context.Context, args struct {
	BaseURL *string
	APIKey  *string
}) ([]ModelInfo, error) {
	if (args.BaseURL != nil && len(*args.BaseURL) > 4096) || (args.APIKey != nil && len(*args.APIKey) > 4096) {
		return nil, codedError{errors.New("invalid model endpoint settings"), "INVALID_MODEL_BASE_URL"}
	}
	models, err := r.app.FetchModels(ctx, args.BaseURL, args.APIKey)
	var invalid *settings.ValidationError
	if errors.As(err, &invalid) {
		return nil, codedError{err, invalid.Code}
	}
	if err != nil {
		r.logger.Error("fetch models", "error", err)
		return nil, codedError{errors.New("could not fetch model catalog"), "MODEL_FETCH_FAILED"}
	}
	return models, nil
}

type imageInput struct{ Name, MediaType, Data string }

type payloadInput struct {
	Images     *[]imageInput
	Model      *string
	Permission *string
	Thinking   *string
	MCP        *string
	MCPServers *[]string
	JSONRender *bool
}

func (p *payloadInput) payload() run.Payload {
	if p == nil {
		return run.Payload{}
	}
	payload := run.Payload{Options: run.Options{
		Model: value(p.Model), Permission: value(p.Permission), Thinking: value(p.Thinking), MCP: value(p.MCP),
		JSONRender: p.JSONRender != nil && *p.JSONRender,
	}}
	if p.MCPServers != nil {
		payload.Options.MCPServers = *p.MCPServers
		payload.Options.MCP = ""
	}
	if p.Images != nil {
		payload.Images = make([]run.Image, len(*p.Images))
		for i, image := range *p.Images {
			payload.Images[i] = run.Image(image)
		}
	}
	return payload
}

func value(text *string) string {
	if text == nil {
		return ""
	}
	return *text
}

func (r *resolver) SubmitRun(ctx context.Context, args struct {
	SessionID gql.ID
	Prompt    string
	Payload   *payloadInput
	Queue     *bool
}) (runResolver, error) {
	item, err := r.service.Submit(ctx, auth.CurrentUser(ctx).ID, string(args.SessionID), args.Prompt,
		args.Payload.payload(), args.Queue != nil && *args.Queue)
	if err != nil {
		return runResolver{}, r.apiError(err)
	}
	return runResolver{item}, nil
}

func (r *resolver) StopRun(ctx context.Context, args struct{ SessionID, RunID gql.ID }) (bool, error) {
	if err := r.service.Stop(ctx, auth.CurrentUser(ctx).ID, string(args.SessionID), string(args.RunID)); err != nil {
		return false, r.apiError(err)
	}
	return true, nil
}

type retryOptionsInput struct {
	Model, Permission, Thinking string
	MCPServers                  []string
	JSONRender                  *bool
}

func (r *resolver) RetryTurn(ctx context.Context, args struct {
	SessionID, TurnID gql.ID
	Model             *string
	Options           *retryOptionsInput
}) (runResolver, error) {
	retry := run.RetryOptions{Model: value(args.Model)}
	if input := args.Options; input != nil {
		retry.Selection = &run.Options{Model: input.Model, Permission: input.Permission, Thinking: input.Thinking,
			MCPServers: input.MCPServers, JSONRender: input.JSONRender != nil && *input.JSONRender}
	}
	item, err := r.service.RetryTurn(ctx, auth.CurrentUser(ctx).ID, string(args.SessionID), string(args.TurnID), retry)
	if err != nil {
		return runResolver{}, r.apiError(err)
	}
	return runResolver{item}, nil
}

type revertedTurnResolver struct {
	Session summaryResolver
	Prompt  string
	Images  []run.Image
}

func (r *resolver) RevertTurn(ctx context.Context, args struct{ SessionID, TurnID, ExpectedHeadID gql.ID }) (revertedTurnResolver, error) {
	result, err := r.service.RevertTurn(ctx, auth.CurrentUser(ctx).ID, string(args.SessionID), string(args.TurnID), string(args.ExpectedHeadID))
	if err != nil {
		return revertedTurnResolver{}, r.apiError(err)
	}
	return revertedTurnResolver{Session: summaryResolver{result.Session}, Prompt: result.Prompt, Images: append([]run.Image{}, result.Images...)}, nil
}

func (r *resolver) SteerRun(ctx context.Context, args struct {
	SessionID, RunID gql.ID
	Prompt           string
}) (bool, error) {
	if err := r.service.Steer(auth.CurrentUser(ctx).ID, string(args.SessionID), string(args.RunID), args.Prompt); err != nil {
		return false, r.apiError(err)
	}
	return true, nil
}
