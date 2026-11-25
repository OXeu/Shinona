// Package pigo adapts the pi-go agent runtime to an application run.
package pigo

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/guanshan/pi-go/packages/agent"
	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type ToolSource interface {
	Open(ctx context.Context, owner string, names []string) ([]agent.AgentTool, func() error, error)
}

type Agent struct {
	model    ai.Model
	endpoint *Endpoint
	apiKey   string
	remote   ToolSource
}

func New(endpoint *Endpoint, modelID string, remote ToolSource) *Agent {
	return &Agent{endpoint: endpoint, model: endpoint.Model(modelID), apiKey: endpoint.key, remote: remote}
}

func (a *Agent) Run(ctx context.Context, input run.Input, workspace sandbox.Workspace) (run.Result, error) {
	model := a.model
	options := input.Payload.Options
	if a.endpoint != nil {
		if options.Model != "" {
			model.ID = options.Model
		}
		model = a.endpoint.Model(model.ID)
	}
	result := run.Result{ContextWindow: model.ContextWindow}
	if len(input.Payload.Images) > 0 && !slices.Contains(model.Input, "image") {
		return result, errors.New("the selected model does not accept images")
	}

	systemPrompt := "You are a coding assistant. Use the provided tools for the sandbox workspace. " +
		"Tool failures are recoverable. Use any partial results to continue or answer with the evidence you have."
	toolset := tools(workspace, options.Permission != run.ReadOnly)
	if options.ArrowJS {
		toolset = append(toolset, arrowRenderTool())
		systemPrompt += " Use render_ui when a custom visualization or an interactive view would make the answer clearer."
	}
	if names := options.SelectedMCPServers(); len(names) > 0 {
		if a.remote == nil || options.Permission == run.ReadOnly {
			return result, run.ErrOptions
		}
		remoteTools, closeTools, err := a.remote.Open(ctx, input.OwnerID, names)
		if err != nil {
			return result, err
		}
		defer func() { _ = closeTools() }()
		toolset = append(toolset, remoteTools...)
	}
	for index, tool := range toolset {
		toolset[index] = withToolTimeout(tool)
	}
	var reasoning ai.ThinkingLevel
	if options.Thinking != "" && options.Thinking != "off" {
		reasoning = ai.ThinkingLevel(options.Thinking)
		model.Reasoning = true
		model.Compat.SupportsReasoningEffort = new(true)
	}
	history, err := conversationHistory(input.History, model)
	if err != nil {
		return result, fmt.Errorf("load agent messages: %w", err)
	}
	parts := turnParts{report: input.ReportParts, toolIndex: make(map[string]int)}
	stream := agent.DefaultStreamFn(nil)
	if a.endpoint != nil {
		stream = func(ctx context.Context, model ai.Model, conversation ai.Context, options ai.StreamOptions) agent.AssistantStream {
			return a.endpoint.stream(ctx, model, conversation, options, input.ReportContext)
		}
	}
	messages, err := agent.RunAgentLoop(ctx, []agent.AgentMessage{ai.NewUserMessage(input.Prompt, imageBlocks(input.Payload.Images))}, agent.AgentContext{
		SystemPrompt: systemPrompt,
		Tools:        toolset,
		Messages:     history,
	}, agent.AgentLoopConfig{
		Model:               model,
		Reasoning:           reasoning,
		GetAPIKey:           func(string) (string, error) { return a.apiKey, nil },
		ToolExecution:       agent.ToolExecutionSequential,
		GetSteeringMessages: collect(input.Steering.Drain),
		GetFollowUpMessages: collect(input.Steering.Finish),
	}, parts.event, stream)
	result.Parts = parts.snapshot()
	var usage run.TokenUsage
	for _, entry := range messages {
		if message, ok := ai.AsAssistantMessage(entry); ok {
			if used := contextTokens(message.Usage); used > 0 {
				result.ContextTokens = used
			}
			usage.Input += int32(message.Usage.Input + message.Usage.CacheWrite)
			usage.Cached += int32(message.Usage.CacheRead)
			usage.Output += int32(message.Usage.Output)
		}
	}
	if usage != (run.TokenUsage{}) {
		result.TokenUsage = &usage
	}
	var encodeErr error
	result.AgentMessages, encodeErr = encodeTurnMessages(messages)
	if encodeErr != nil {
		return result, errors.Join(err, fmt.Errorf("save agent messages: %w", encodeErr))
	}
	if err != nil {
		return result, fmt.Errorf("%w: %w", run.ErrModelResponse, err)
	}
	if len(messages) == 0 {
		return result, fmt.Errorf("%w: pi-go returned no messages", run.ErrModelResponse)
	}
	message, ok := ai.AsAssistantMessage(messages[len(messages)-1])
	if !ok || message.StopReason == "error" || message.StopReason == "aborted" {
		if message.ErrorMessage != "" {
			return result, fmt.Errorf("%w: %s", run.ErrModelResponse, message.ErrorMessage)
		}
		return result, run.ErrModelResponse
	}
	result.Output = ai.MessageText(message)
	if result.Output == "" {
		if index := slices.IndexFunc(result.Parts, func(part run.TurnPart) bool { return part.Kind == "ui" }); index >= 0 {
			result.Output = result.Parts[index].Text
		}
	}
	if result.Output == "" {
		return result, fmt.Errorf("%w: the model returned an empty answer", run.ErrModelResponse)
	}
	// The transcript displays the final assistant text from Output.
	result.Parts = append(result.Parts[:parts.finalStart], slices.DeleteFunc(result.Parts[parts.finalStart:], func(part run.TurnPart) bool {
		return part.Kind == "text"
	})...)
	return result, nil
}

func imageBlocks(images []run.Image) []ai.ContentBlock {
	blocks := make([]ai.ContentBlock, 0, len(images))
	for _, image := range images {
		blocks = append(blocks, ai.ContentBlock{Type: "image", Data: image.Data, MimeType: image.MediaType})
	}
	return blocks
}

func collect(read func() []string) func(context.Context) ([]agent.AgentMessage, error) {
	return func(ctx context.Context) ([]agent.AgentMessage, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		text := read()
		messages := make([]agent.AgentMessage, 0, len(text))
		for _, message := range text {
			messages = append(messages, ai.NewUserMessage(message, nil))
		}
		return messages, nil
	}
}

var _ run.Agent = (*Agent)(nil)
