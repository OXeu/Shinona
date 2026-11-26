package pigo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"github.com/OXeu/shinona/internal/run"
)

type sessionExecution struct {
	messages        []agent.AgentMessage
	compacted       []agent.AgentMessage
	afterCompaction []agent.AgentMessage
}

func (a *Agent) runSession(ctx context.Context, input run.Input, model ai.Model, reasoning agent.ThinkingLevel,
	loadout toolLoadout, history sessionHistory,
) (sessionExecution, error) {
	options := coding.SessionOptions{
		Model: &model, Cwd: "/", Tools: loadout.tools, SystemPrompt: loadout.prompt,
		ThinkingLevel: reasoning, Compaction: coding.DefaultCompactionSettings(),
	}
	if input.OwnerID != "" && input.SessionID != "" {
		// Keep provider affinity across turns without sending the app's IDs.
		digest := sha256.Sum256([]byte(input.OwnerID + "\x00" + input.SessionID))
		options.SessionID = hex.EncodeToString(digest[:])
	}
	if a.endpoint != nil {
		options.StreamFn = func(ctx context.Context, model *ai.Model, conversation ai.TranscriptContext, options *ai.SimpleStreamOptions) *ai.AssistantMessageEventStream {
			return a.endpoint.stream(ctx, model, conversation, options, input.ReportContext)
		}
	}
	session, err := coding.NewSession(options)
	if err != nil {
		return sessionExecution{}, fmt.Errorf("create Pi session: %w", err)
	}

	engine := session.Agent
	engine.SetThinkingLevel(reasoning)
	engine.ToolExecution = agent.ToolSequential

	// Prompt the engine with the sandbox's explicit instructions. The coding
	// session's CLI prompt builder also reads context and skills from the host.
	declarations := make([]ai.Tool, 0, len(loadout.tools))
	for _, tool := range loadout.tools {
		declarations = append(declarations, ai.ToToolDeclaration(ai.Tool{
			Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters, ConstrainedSampling: tool.ConstrainedSampling,
		}))
	}
	prompt, _ := ai.CreateInitialSystemMessage(loadout.prompt, declarations)
	initial := history.load(session, prompt)

	// Pi reapplies each checkpoint to subsequent requests. Record its stamped
	// system message once so later model responses stay after the boundary.
	transform := engine.TransformContext
	checkpointTime := prompt.Timestamp
	var projection []agent.AgentMessage
	var projectedThrough int
	engine.TransformContext = func(ctx context.Context, messages []agent.AgentMessage) []agent.AgentMessage {
		projected := transform(ctx, messages)
		if head, ok := ai.GetInitialSystemMessage(projected); ok && head.Timestamp != checkpointTime {
			checkpointTime = head.Timestamp
			projection, projectedThrough = projected, len(messages)
		}
		return projected
	}

	// Feed delivered steering into Pi as normal user messages. PrepareNextTurn
	// replaces the CLI prompt refresh with the complete batch, in queue order.
	var pending []agent.AgentMessage
	engine.PrepareNextTurn = func(agent.AgentTurnContext) *agent.AgentLoopTurnUpdate {
		update := &agent.AgentLoopTurnUpdate{Messages: pending}
		pending = nil
		return update
	}
	engine.FinishTurn = func(ctx context.Context, turn agent.AgentTurnContext) agent.AgentTurnDecision {
		if ctx.Err() != nil || !successfulResponse(turn.Message) {
			return ""
		}
		if len(turn.ToolResults) > 0 {
			pending = steeringMessages(input.Steering.Drain())
		} else {
			pending = steeringMessages(input.Steering.Finish(ctx))
		}
		if len(pending) > 0 {
			return agent.TurnContinue
		}
		return ""
	}
	reportLive := liveParts(input.ReportParts)
	defer session.Subscribe(func(_ context.Context, event agent.AgentEvent) error {
		reportLive(event)
		return nil
	})()
	content := ai.ContentList{ai.TextContent{Text: input.Prompt}}
	for _, image := range imageBlocks(input.Payload.Images) {
		content = append(content, image)
	}
	prompts := []agent.AgentMessage{ai.UserMessage{Content: content, Timestamp: time.Now().UnixMilli()}}
	prompts = append(prompts, steeringMessages(input.Steering.Drain())...)
	err = engine.PromptMessages(ctx, prompts)
	transcript := session.History()
	execution := sessionExecution{messages: transcript[initial:]}
	if projection != nil {
		execution.compacted = projection
		execution.afterCompaction = transcript[projectedThrough:]
	}
	return execution, err
}

func steeringMessages(messages []string) []agent.AgentMessage {
	if len(messages) == 0 {
		return nil
	}
	timestamp := time.Now().UnixMilli()
	result := make([]agent.AgentMessage, len(messages))
	for i, message := range messages {
		result[i] = ai.NewUserText(message, timestamp)
	}
	return result
}
