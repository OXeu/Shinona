package rest

import (
	"context"
	"errors"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/settings"
)

type ComposerOptions struct {
	DefaultModel  string                `json:"defaultModel"`
	Models        []ModelInfo           `json:"models"`
	ModelsLoading bool                  `json:"modelsLoading"`
	MCPServers    []string              `json:"mcpServers"`
	Defaults      *run.ComposerDefaults `json:"defaults"`
}

type ModelInfo struct {
	ID            string `json:"id"`
	ContextWindow int32  `json:"contextWindow"`
}

func (r *handler) ComposerOptions(ctx context.Context) (ComposerOptions, error) {
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

func (r *handler) SetComposerDefaults(ctx context.Context, input run.ComposerDefaults) (run.ComposerDefaults, error) {
	if err := r.service.SetComposerDefaults(ctx, auth.CurrentUser(ctx).ID, input); err != nil {
		return run.ComposerDefaults{}, r.apiError(err)
	}
	return input, nil
}

func (r *handler) ModelSettings(ctx context.Context) ([]settings.Field, error) {
	fields, err := r.app.ModelFields(ctx)
	if err != nil {
		return nil, r.apiError(err)
	}
	return fields, nil
}

func (r *handler) UpdateModelSettings(ctx context.Context, args struct{ Changes []settingChange }) ([]settings.Field, error) {
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

func (r *handler) FetchModels(ctx context.Context, args struct {
	BaseURL *string
	APIKey  *string
}) ([]ModelInfo, error) {
	const maxModelFieldBytes = 4096
	if args.BaseURL != nil && len(*args.BaseURL) > maxModelFieldBytes {
		return nil, codedError{errors.New("the model base URL exceeds 4096 bytes"), "INVALID_MODEL_BASE_URL"}
	}
	if args.APIKey != nil && len(*args.APIKey) > maxModelFieldBytes {
		return nil, codedError{errors.New("the model API key exceeds 4096 bytes"), "INVALID_MODEL_API_KEY"}
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
	ArrowJS    *bool
}

func (p *payloadInput) payload() run.Payload {
	if p == nil {
		return run.Payload{}
	}
	payload := run.Payload{Options: run.Options{
		Model: value(p.Model), Permission: value(p.Permission), Thinking: value(p.Thinking), MCP: value(p.MCP),
		ArrowJS: p.ArrowJS != nil && *p.ArrowJS,
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

func (r *handler) SubmitRun(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	Prompt    string
	Payload   *payloadInput
	Queue     *bool
}) (turnResponse, error) {
	item, err := r.service.Submit(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.Prompt,
		args.Payload.payload(), args.Queue != nil && *args.Queue)
	if err != nil {
		return turnResponse{}, r.apiError(err)
	}
	return publicTurn(item), nil
}

func (r *handler) StopRun(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	RunID     string `uri:"turnId" json:"-"`
}) error {
	if err := r.service.Stop(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.RunID); err != nil {
		return r.apiError(err)
	}
	return nil
}

type retryOptionsInput struct {
	Model, Permission, Thinking string
	MCPServers                  []string
	ArrowJS                     *bool
}

func (r *handler) RetryTurn(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	TurnID    string `uri:"turnId" json:"-"`
	Model     *string
	Options   *retryOptionsInput
}) (turnResponse, error) {
	retry := run.RetryOptions{Model: value(args.Model)}
	if input := args.Options; input != nil {
		retry.Selection = &run.Options{Model: input.Model, Permission: input.Permission, Thinking: input.Thinking,
			MCPServers: input.MCPServers, ArrowJS: input.ArrowJS != nil && *input.ArrowJS}
	}
	item, err := r.service.RetryTurn(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.TurnID, retry)
	if err != nil {
		return turnResponse{}, r.apiError(err)
	}
	return publicTurn(item), nil
}

type revertedTurnResponse struct {
	Session sessionSummary `json:"session"`
	Prompt  string         `json:"prompt"`
	Images  []run.Image    `json:"images"`
}

func (r *handler) RevertTurn(ctx context.Context, args struct {
	SessionID      string `uri:"id" json:"-"`
	TurnID         string `uri:"turnId" json:"-"`
	ExpectedHeadID string
}) (revertedTurnResponse, error) {
	result, err := r.service.RevertTurn(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.TurnID, args.ExpectedHeadID)
	if err != nil {
		return revertedTurnResponse{}, r.apiError(err)
	}
	return revertedTurnResponse{Session: publicSession(result.Session), Prompt: result.Prompt, Images: append([]run.Image{}, result.Images...)}, nil
}

func (r *handler) SteerRun(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	RunID     string `uri:"turnId" json:"-"`
	Prompt    string
}) error {
	if err := r.service.Steer(auth.CurrentUser(ctx).ID, args.SessionID, args.RunID, args.Prompt); err != nil {
		return r.apiError(err)
	}
	return nil
}
