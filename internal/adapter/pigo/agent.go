// Package pigo adapts the sky-valley/pi agent runtime to an application run.
package pigo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

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
	return &Agent{endpoint: endpoint, model: endpoint.Model(modelID), remote: remote}
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
		for _, remote := range remoteTools {
			tool, err := remoteAgentTool(remote)
			if err != nil {
				return result, err
			}
			toolset = append(toolset, tool)
		}
	}
	for index, tool := range toolset {
		toolset[index] = withToolTimeout(tool)
	}
	var reasoning agent.ThinkingLevel
	if options.Thinking != "" && options.Thinking != "off" {
		reasoning = agent.ThinkingLevel(options.Thinking)
		model.Reasoning = true
		model.Compat = completionCompatibility(model.Compat, false, true)
	}
	history, err := conversationHistory(input.History, model)
	if err != nil {
		return result, fmt.Errorf("load agent messages: %w", err)
	}
	reportLive := liveParts(input.ReportParts)
	var stream agent.StreamFn
	if a.endpoint != nil {
		stream = func(ctx context.Context, model *ai.Model, conversation ai.TranscriptContext, options *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return a.endpoint.stream(ctx, model, conversation, options, input.ReportContext)
		}
	}
	session, err := coding.NewSession(coding.SessionOptions{
		Model:      &model,
		Cwd:        "/",
		NoTools:    coding.NoToolsAll,
		StreamFn:   stream,
		Compaction: coding.DefaultCompactionSettings(),
	})
	if err != nil {
		return result, fmt.Errorf("create Pi session: %w", err)
	}
	// Reuse Pi's coding context transform with the sandbox tools and prompt.
	// The coding session's default prompt discovers resources on the host.
	transform := session.Agent.TransformContext
	var compacted []agent.AgentMessage
	var compactedThrough int
	engine := agent.NewAgent(agent.AgentOptions{
		InitialState: &agent.AgentState{
			Model: &model, ThinkingLevel: reasoning, SystemPrompt: systemPrompt, Tools: toolset, Messages: history,
		},
		ToolExecution: agent.ToolSequential,
		SteeringMode:  agent.QueueAll,
		StreamFn:      session.Agent.StreamFn,
		Identity:      &ai.ClientIdentity{Attribution: ai.NoAttribution},
		TransformContext: func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage {
			projected := transform(ctx, messages)
			// Pi returns the original slice until it produces a projection.
			if len(projected) > 0 && len(messages) > 0 && &projected[0] != &messages[0] {
				compacted, compactedThrough = projected, len(messages)
			}
			return projected
		},
	})
	session.Agent = engine
	initial := len(engine.State().Messages)
	steer := func(messages []string) {
		for _, text := range messages {
			engine.Steer(ai.NewUserText(text, time.Now().UnixMilli()))
		}
	}
	defer engine.Subscribe(func(ctx context.Context, event agent.AgentEvent) error {
		reportLive(event)
		if event.Type == agent.EvTurnEnd && ctx.Err() == nil {
			if message := assistantMessage(event.Message); message != nil &&
				message.StopReason != ai.StopError && message.StopReason != ai.StopAborted {
				if len(event.ToolResults) > 0 {
					steer(input.Steering.Drain())
				} else {
					steer(input.Steering.Finish())
				}
			}
		}
		return nil
	})()
	if err := ctx.Err(); err != nil {
		return result, err
	}
	steer(input.Steering.Drain())
	err = engine.Prompt(ctx, input.Prompt, imageBlocks(input.Payload.Images)...)
	transcript := engine.State().Messages
	messages := transcript[initial:]
	var message *ai.AssistantMessage
	if len(messages) > 0 {
		message = assistantMessage(messages[len(messages)-1])
	}
	answered := err == nil && message != nil && message.StopReason != ai.StopError && message.StopReason != ai.StopAborted
	result.Parts = transcriptParts(messages, answered)
	var usage run.TokenUsage
	for _, entry := range messages {
		if message := assistantMessage(entry); message != nil {
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
	result.AgentMessages, encodeErr = encodeMessages(messages, false)
	if encodeErr == nil && compacted != nil {
		result.CompactedContext, encodeErr = encodeMessages(slices.Concat(compacted, transcript[compactedThrough:]), true)
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

func imageBlocks(images []run.Image) []ai.ImageContent {
	blocks := make([]ai.ImageContent, 0, len(images))
	for _, image := range images {
		blocks = append(blocks, ai.ImageContent{Data: image.Data, MimeType: image.MediaType})
	}
	return blocks
}

var _ run.Agent = (*Agent)(nil)
