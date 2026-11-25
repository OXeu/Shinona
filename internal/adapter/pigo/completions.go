package pigo

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/guanshan/pi-go/packages/agent"
	"github.com/guanshan/pi-go/packages/ai"
	"github.com/guanshan/pi-go/packages/ai/providers"
	"github.com/guanshan/pi-go/packages/ai/providers/openaichat"

	"github.com/OXeu/shinona/internal/run"
)

// Keep pi-go's request formatting and response parsing. Own the HTTP request
// because pi-go's built-in provider applies a deadline even without one in its options.
func (e *Endpoint) stream(ctx context.Context, model ai.Model, input ai.Context, options ai.StreamOptions,
	onContext func(run.ContextUsage),
) agent.AssistantStream {
	events := ai.NewAssistantMessageEventStreamWithContext(ctx, 16)
	go func() {
		var last int
		report := func(usage ai.Usage) {
			tokens := contextTokens(usage)
			if onContext == nil || tokens <= 0 || tokens == last || ctx.Err() != nil {
				return
			}
			last = tokens
			onContext(run.ContextUsage{Tokens: int32(tokens), Window: int32(model.ContextWindow)})
		}
		message, err := e.complete(ctx, model, input, options, events, report)
		report(message.Usage)
		if err != nil {
			if message.Model == "" {
				message = ai.NewAssistantMessageForModel(model, nil, ai.Usage{}, "error")
			}
			message.StopReason = "error"
			message.ErrorMessage = err.Error()
		}
		if e.key != "" && message.ErrorMessage != "" {
			message.ErrorMessage = strings.ReplaceAll(message.ErrorMessage, e.key, "[redacted]")
		}
		if message.StopReason == "error" || message.StopReason == "aborted" {
			events.Push(ai.AssistantMessageEvent{Type: "error", Reason: message.StopReason, Error: message})
			return
		}
		events.Push(ai.AssistantMessageEvent{Type: "done", Reason: message.StopReason, Message: message})
	}()
	return events
}

func (e *Endpoint) complete(ctx context.Context, model ai.Model, input ai.Context, options ai.StreamOptions,
	events *ai.AssistantMessageEventStream, report func(ai.Usage),
) (ai.AssistantMessage, error) {
	compat := ai.GetOpenAICompletionsCompat(model)
	request := providers.BuildOpenAIChatRequest(e.key, providers.OpenAIChatRequestOptions{
		ModelID: model.ID, Provider: model.Provider, BaseURL: model.BaseURL,
		SystemPrompt: input.SystemPrompt, Messages: completionMessages(input.Messages, model),
		Tools: ai.ToolDefinitions(ai.ToolsByName(input.Tools)), SupportsImages: ai.SupportsInput(model, "image"),
		ModelHeaders: model.Headers, RequestHeaders: options.Headers,
		Temperature: options.Temperature, ToolChoice: options.ToolChoice,
		ThinkingLevel: string(options.Reasoning), ThinkingLevelMap: model.ThinkingLevelMap, Reasoning: model.Reasoning,
		CacheRetention: options.CacheRetention, SessionID: options.SessionID,
		SupportsStore: compat.SupportsStore, SupportsDeveloperRole: compat.SupportsDeveloperRole,
		SupportsReasoningEffort: compat.SupportsReasoningEffort, CacheControlFormat: compat.CacheControlFormat,
		SendSessionAffinityHeaders: compat.SendSessionAffinityHeaders,
		SupportsLongCacheRetention: new(compat.SupportsLongCacheRetention),
		RequiresToolResultName:     compat.RequiresToolResultName, RequiresAssistantAfterToolResult: compat.RequiresAssistantAfterToolResult,
		RequiresThinkingAsText:                      compat.RequiresThinkingAsText,
		RequiresReasoningContentOnAssistantMessages: compat.RequiresReasoningContentOnAssistantMessages,
		ThinkingFormat:                              compat.ThinkingFormat, ZaiToolStream: compat.ZaiToolStream, SupportsStrictMode: compat.SupportsStrictMode,
		OpenRouterRouting: compat.OpenRouterRouting, VercelGatewayRouting: compat.VercelGatewayRouting,
	})
	request.Body["stream"] = true
	request.Body["stream_options"] = map[string]bool{"include_usage": true}
	body, err := providers.MarshalJSON(request.Body)
	if err != nil {
		return ai.AssistantMessage{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, providers.OpenAIChatURL(model.BaseURL), bytes.NewReader(body))
	if err != nil {
		return ai.AssistantMessage{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream, application/json")
	if request.BearerAuth && request.Key != "" {
		req.Header.Set("Authorization", "Bearer "+request.Key)
	}
	for key, value := range request.Headers {
		req.Header.Set(key, value)
	}
	client := *e.client
	client.Timeout = 0
	response, err := client.Do(req)
	if err != nil {
		return ai.AssistantMessage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		details, _ := io.ReadAll(io.LimitReader(response.Body, int64(4096+len(e.key))))
		return ai.AssistantMessage{}, fmt.Errorf("model HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(details)))
	}
	if !strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "event-stream") {
		raw, err := io.ReadAll(response.Body)
		if err != nil {
			return ai.AssistantMessage{}, err
		}
		parsed, err := providers.ParseOpenAIChatCompletionRawForProvider(raw, model.Provider)
		return completionMessage(model, parsed), err
	}
	return readCompletionStream(model, response.Body, events, report)
}

func readCompletionStream(model ai.Model, body io.Reader, events *ai.AssistantMessageEventStream,
	report func(ai.Usage),
) (ai.AssistantMessage, error) {
	accumulator := openaichat.NewStreamAccumulator(model.Provider)
	partial := ai.NewAssistantMessageForModel(model, nil, ai.Usage{}, "stop")
	events.Push(ai.AssistantMessageEvent{Type: "start", Partial: partial})
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), 8<<20)
	for scanner.Scan() {
		line, dataLine := strings.CutPrefix(scanner.Text(), "data:")
		if !dataLine {
			continue
		}
		line = strings.TrimSpace(line)
		if line == "[DONE]" {
			break
		}
		if line == "" {
			continue
		}
		frame := []byte(line)
		updates, err := accumulator.ApplyRaw(frame)
		if err != nil {
			return partial, err
		}
		// Providers can send usage in its own chunk, without any text delta.
		if len(updates) > 0 || bytes.Contains(frame, []byte(`"usage"`)) {
			partial = completionMessage(model, accumulator.Parsed(false))
			report(partial.Usage)
		}
		for _, update := range updates {
			events.Push(ai.AssistantMessageEvent{Type: update.Type, ContentIndex: update.ContentIndex, Delta: update.Delta, Partial: partial})
		}
	}
	if err := scanner.Err(); err != nil {
		return partial, err
	}
	message := completionMessage(model, accumulator.Parsed(true))
	if !accumulator.SawFinishReason() {
		return message, fmt.Errorf("model stream ended without a finish reason")
	}
	return message, nil
}

func completionMessages(messages []ai.Message, model ai.Model) []providers.OpenAIChatMessage {
	result := make([]providers.OpenAIChatMessage, 0, len(messages))
	var pending []ai.ContentBlock
	finishToolCalls := func() {
		// A turn can stop after the model requests tools but before the results
		// are appended. Chat Completions requires a result for every call.
		for _, call := range pending {
			result = append(result, providers.OpenAIChatMessage{
				Role: "toolResult", ToolCallID: call.ID, ToolName: call.Name,
				Blocks: []providers.OpenAIChatMessageBlock{{Type: "text", Text: "No result provided"}},
			})
		}
		pending = pending[:0]
	}
	for _, message := range messages {
		role := ai.MessageRole(message)
		if role == "toolResult" {
			id := ai.MessageToolCallID(message)
			pending = slices.DeleteFunc(pending, func(call ai.ContentBlock) bool { return call.ID == id })
		} else {
			finishToolCalls()
		}
		assistant, isAssistant := ai.AsAssistantMessage(message)
		sameModel := assistant.Provider == model.Provider && assistant.API == model.API && assistant.Model == model.ID
		content := ai.MessageBlocks(message)
		blocks := make([]providers.OpenAIChatMessageBlock, 0, len(content))
		for _, block := range content {
			// Follow pi-go's provider history rules for model-specific reasoning.
			if isAssistant && !sameModel {
				switch block.Type {
				case "thinking":
					if block.Redacted || block.Thinking == "" {
						continue
					}
					block = ai.ContentBlock{Type: "text", Text: block.Thinking}
				case "toolCall":
					block.ThoughtSignature = ""
				}
			}
			if block.Type == "toolCall" {
				pending = append(pending, block)
			}
			blocks = append(blocks, providers.OpenAIChatMessageBlock{
				Type: block.Type, Text: block.Text, MimeType: block.MimeType, Data: block.Data, Thinking: block.Thinking,
				ID: block.ID, Name: block.Name, Arguments: block.Arguments,
				ThinkingSignature: cmp.Or(block.Signature, block.ThinkingSignature), ThoughtSignature: block.ThoughtSignature,
			})
		}
		result = append(result, providers.OpenAIChatMessage{
			Role: role, Text: ai.MessageText(message), Blocks: blocks,
			ToolCallID: ai.MessageToolCallID(message), ToolName: ai.MessageToolName(message),
		})
	}
	finishToolCalls()
	return result
}

func completionMessage(model ai.Model, parsed providers.OpenAIChatParsed) ai.AssistantMessage {
	blocks := make([]ai.ContentBlock, 0, len(parsed.Blocks))
	for _, block := range parsed.Blocks {
		blocks = append(blocks, ai.ContentBlock{
			Type: block.Type, Text: block.Text, Thinking: block.Thinking, Signature: block.ThinkingSignature,
			ID: block.ID, Name: block.Name, Arguments: block.Arguments, ThoughtSignature: block.ThoughtSignature,
		})
	}
	usage := ai.Usage{
		Input: parsed.Usage.Input, Output: parsed.Usage.Output,
		CacheRead: parsed.Usage.CacheRead, CacheWrite: parsed.Usage.CacheWrite, TotalTokens: parsed.Usage.TotalTokens,
	}
	usage.Cost = ai.CalculateCost(model, usage)
	message := ai.NewAssistantMessageForModel(model, blocks, usage, parsed.StopReason)
	message.ErrorMessage, message.ResponseID = parsed.ErrorMessage, parsed.ResponseID
	if parsed.ResponseModel != model.ID {
		message.ResponseModel = parsed.ResponseModel
	}
	return message
}
