// Package pigo adapts the sky-valley/pi agent runtime to an application run.
package pigo

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type ToolSource interface {
	Open(ctx context.Context, owner string, names []string) ([]run.Tool, func() error, error)
}

type Agent struct {
	model    ai.Model
	endpoint *Endpoint
	remote   ToolSource
}

func New(endpoint *Endpoint, modelID string, remote ToolSource) *Agent {
	return &Agent{endpoint: endpoint, model: ai.Model{ID: modelID}, remote: remote}
}

func (a *Agent) Run(ctx context.Context, input run.Input, workspace sandbox.Workspace) (run.Result, error) {
	model, reasoning := a.selectedModel(input.Payload.Options)
	result := run.Result{ContextWindow: model.ContextWindow}
	if len(input.Payload.Images) > 0 && !slices.Contains(model.Input, "image") {
		return result, fmt.Errorf("%w: the selected model does not accept images", run.ErrModelResponse)
	}
	loadout, closeTools, err := a.openTools(ctx, input, workspace)
	if err != nil {
		return result, err
	}
	defer closeTools()

	history, err := conversationHistory(input.History, model)
	if err != nil {
		return result, fmt.Errorf("load agent messages: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	execution, err := a.runSession(ctx, input, model, reasoning, loadout, history)
	return turnResult(ctx, result, execution, err)
}

func (a *Agent) selectedModel(options run.Options) (ai.Model, agent.ThinkingLevel) {
	model := a.model
	reasoning := agent.ThinkOff
	if options.Thinking != "" && options.Thinking != "off" {
		reasoning = agent.ThinkingLevel(options.Thinking)
	}
	if a.endpoint != nil {
		if options.Model != "" {
			model.ID = options.Model
		}
		return a.endpoint.model(model.ID, reasoning != agent.ThinkOff), reasoning
	}
	if reasoning != agent.ThinkOff {
		model.Reasoning = true
		model.Compat = completionCompatibility(model.Compat, false, true)
	}
	return model, reasoning
}

type toolLoadout struct {
	prompt string
	tools  []agent.AgentTool
}

func (a *Agent) openTools(ctx context.Context, input run.Input, workspace sandbox.Workspace) (toolLoadout, func(), error) {
	options := input.Payload.Options
	loadout := toolLoadout{
		prompt: "You are a coding assistant. Use the provided tools for the sandbox workspace. " +
			"Tool failures are recoverable. Use any partial results to continue or answer with the evidence you have.",
		tools: tools(workspace, options.Permission != run.ReadOnly),
	}
	if options.ArrowJS {
		loadout.tools = append(loadout.tools, arrowRenderTool())
		loadout.prompt += " Use render_ui when a custom visualization or an interactive view would make the answer clearer."
	}
	closeTools := func() {}
	if names := options.SelectedMCPServers(); len(names) > 0 {
		if a.remote == nil || options.Permission == run.ReadOnly {
			return toolLoadout{}, nil, run.ErrOptions
		}
		remoteTools, release, err := a.remote.Open(ctx, input.OwnerID, names)
		if err != nil {
			return toolLoadout{}, nil, err
		}
		closeTools = func() { _ = release() }
		for _, remote := range remoteTools {
			tool, err := remoteAgentTool(remote)
			if err != nil {
				closeTools()
				return toolLoadout{}, nil, err
			}
			loadout.tools = append(loadout.tools, tool)
		}
	}
	for index, tool := range loadout.tools {
		loadout.tools[index] = withToolTimeout(tool)
	}
	return loadout, closeTools, nil
}

func turnResult(ctx context.Context, result run.Result, execution sessionExecution, err error) (run.Result, error) {
	messages := execution.messages
	var message *ai.AssistantMessage
	if len(messages) > 0 {
		message = assistantMessage(messages[len(messages)-1])
	}
	if err == nil && message != nil && message.StopReason == ai.StopAborted {
		err = ctx.Err()
	}
	answered := err == nil && successfulResponse(message)
	result.Parts = transcriptParts(messages, answered)
	var usage run.TokenUsage
	for _, entry := range messages {
		if message := assistantMessage(entry); message != nil {
			if used := contextTokens(message.Usage); used > 0 {
				result.ContextTokens = used
			}
			usage.Input = addTokenCount(usage.Input, message.Usage.Input)
			usage.Input = addTokenCount(usage.Input, message.Usage.CacheWrite)
			usage.Cached = addTokenCount(usage.Cached, message.Usage.CacheRead)
			usage.Output = addTokenCount(usage.Output, message.Usage.Output)
		}
	}
	if usage != (run.TokenUsage{}) {
		result.TokenUsage = &usage
	}
	var encodeErr error
	result.AgentMessages, encodeErr = encodeTurnMessages(messages)
	if encodeErr == nil && execution.compacted != nil {
		result.CompactedContext, encodeErr = encodeCompactedContext(execution.compacted, execution.afterCompaction)
	}
	if encodeErr != nil {
		return result, errors.Join(err, fmt.Errorf("save agent messages: %w", encodeErr))
	}
	if err != nil {
		return result, fmt.Errorf("%w: %w", run.ErrModelResponse, err)
	}
	if len(messages) == 0 {
		return result, fmt.Errorf("%w: pi returned no messages", run.ErrModelResponse)
	}
	if message == nil {
		return result, run.ErrModelResponse
	}
	if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
		if slices.ContainsFunc(message.Diagnostics, func(info ai.Diagnostic) bool {
			return info.Type == modelIdleDiagnostic
		}) {
			return result, run.ErrModelIdle
		}
		if message.ErrorMessage != "" {
			return result, fmt.Errorf("%w: %s", run.ErrModelResponse, message.ErrorMessage)
		}
		return result, run.ErrModelResponse
	}
	result.Output = ai.ContentText(message.Content)
	if result.Output == "" {
		if index := slices.IndexFunc(result.Parts, func(part run.TurnPart) bool { return part.Kind == "ui" }); index >= 0 {
			result.Output = result.Parts[index].Text
		}
	}
	if result.Output == "" {
		return result, fmt.Errorf("%w: the model returned an empty answer", run.ErrModelResponse)
	}
	return result, nil
}

func successfulResponse(message *ai.AssistantMessage) bool {
	return message != nil && message.StopReason != ai.StopError && message.StopReason != ai.StopAborted
}

func imageBlocks(images []run.Image) []ai.ImageContent {
	blocks := make([]ai.ImageContent, 0, len(images))
	for _, image := range images {
		blocks = append(blocks, ai.ImageContent{Data: image.Data, MimeType: image.MediaType})
	}
	return blocks
}

var _ run.Agent = (*Agent)(nil)
