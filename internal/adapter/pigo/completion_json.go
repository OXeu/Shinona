package pigo

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/sky-valley/pi/ai"
)

const maxJSONCompletionBytes = 8 << 20

type jsonCompletionChoice struct {
	FinishReason string                 `json:"finish_reason"`
	Message      *jsonCompletionMessage `json:"message"`
	Usage        *jsonCompletionUsage   `json:"usage"`
}

type jsonCompletionMessage struct {
	Content          string          `json:"content"`
	ReasoningContent json.RawMessage `json:"reasoning_content"`
	Reasoning        json.RawMessage `json:"reasoning"`
	ReasoningText    json.RawMessage `json:"reasoning_text"`
	ReasoningDetails json.RawMessage `json:"reasoning_details"`
	ToolCalls        []*struct {
		ID       string `json:"id"`
		Function struct {
			Name      string `json:"name"`
			Arguments string `json:"arguments"`
		} `json:"function"`
	} `json:"tool_calls"`
}

func decodeJSONCompletion(body io.Reader, model *ai.Model) (*ai.AssistantMessage, error) {
	var content struct {
		ID      string                 `json:"id"`
		Model   string                 `json:"model"`
		Choices []jsonCompletionChoice `json:"choices"`
		Usage   *jsonCompletionUsage   `json:"usage"`
	}
	limited := &io.LimitedReader{R: body, N: maxJSONCompletionBytes + 1}
	decoder := json.NewDecoder(limited)
	err := decoder.Decode(&content)
	if err == nil {
		err = decoder.Decode(new(json.RawMessage))
		if err == nil {
			err = errors.New("more than one JSON value")
		} else if errors.Is(err, io.EOF) {
			err = nil
		}
	}
	if limited.N == 0 {
		return nil, errors.New("model completion exceeds the response size limit")
	}
	if err != nil {
		return nil, fmt.Errorf("decode JSON model completion: %w", err)
	}
	if len(content.Choices) == 0 || content.Choices[0].Message == nil {
		return nil, errors.New("the model returned no completion message")
	}
	choice := content.Choices[0]
	blocks, err := choice.Message.blocks()
	if err != nil {
		return nil, err
	}
	reply := &ai.AssistantMessage{
		Api: model.Api, Provider: model.Provider, Model: model.ID, ResponseID: content.ID,
		Content: blocks, RawStopReason: choice.FinishReason,
	}
	if content.Model != model.ID {
		reply.ResponseModel = content.Model
	}
	if content.Usage == nil {
		content.Usage = choice.Usage
	}
	if content.Usage != nil {
		reply.Usage = content.Usage.toUsage(model)
	}
	reply.StopReason, reply.ErrorMessage = jsonCompletionStop(model, choice)
	return reply, nil
}

func (m jsonCompletionMessage) blocks() (ai.ContentList, error) {
	blocks := make(ai.ContentList, 0, 2+len(m.ToolCalls))
	var reasoning ai.ThinkingContent
	for _, field := range []struct {
		name  string
		value json.RawMessage
	}{
		{"reasoning_content", m.ReasoningContent},
		{"reasoning", m.Reasoning},
		{"reasoning_text", m.ReasoningText},
	} {
		var text string
		if json.Unmarshal(field.value, &text) == nil && text != "" {
			reasoning.Thinking, reasoning.ThinkingSignature = text, field.name
			break
		}
	}
	if len(m.ReasoningDetails) != 0 {
		reasoning.ThinkingSignature = structuredReasoningSignature(m.ReasoningDetails, reasoning.ThinkingSignature)
	}
	if reasoning.Thinking != "" || reasoning.ThinkingSignature != "" {
		blocks = append(blocks, reasoning)
	}
	if m.Content != "" {
		blocks = append(blocks, ai.TextContent{Text: m.Content})
	}
	for _, entry := range m.ToolCalls {
		if entry == nil {
			return nil, errors.New("the model returned a null tool call")
		}
		args, order, issue := ai.ParseToolCallArguments(entry.Function.Arguments)
		blocks = append(blocks, ai.ToolCall{
			ID: entry.ID, Name: entry.Function.Name, Arguments: args, ArgumentsOrder: order, ArgumentsError: issue,
		})
	}
	return blocks, nil
}

// Pi replays a signature only if every entry in the stored array is valid.
func structuredReasoningSignature(raw json.RawMessage, fallback string) string {
	var details []json.RawMessage
	if json.Unmarshal(raw, &details) != nil {
		return fallback
	}
	var valid []json.RawMessage
	for _, detail := range details {
		value, err := ai.DecodeOrderedValue(detail)
		record, ok := value.(ai.OrderedObject)
		if err != nil || !ok {
			continue
		}
		if !matchesDetailField[string](record, "id", true) || !matchesDetailField[string](record, "format", false) ||
			!matchesDetailField[float64](record, "index", false) {
			continue
		}
		kind, _ := record.Get("type")
		var textField string
		switch kind {
		case "reasoning.summary":
			textField = "summary"
		case "reasoning.encrypted":
			textField = "data"
		case "reasoning.text":
			if !matchesDetailField[string](record, "signature", true) {
				continue
			}
			textField = "text"
		default:
			continue
		}
		text, _ := record.Get(textField)
		if _, ok := text.(string); ok {
			valid = append(valid, detail)
		}
	}
	if len(valid) == 0 {
		return fallback
	}
	signature, _ := json.Marshal(valid)
	return string(signature)
}

func matchesDetailField[T any](record ai.OrderedObject, name string, nullable bool) bool {
	value, present := record.Get(name)
	if !present || (value == nil && nullable) {
		return true
	}
	_, ok := value.(T)
	return ok
}

func jsonCompletionStop(model *ai.Model, choice jsonCompletionChoice) (ai.StopReason, string) {
	switch choice.FinishReason {
	case "stop", "end":
		return ai.StopStop, ""
	case "length":
		return ai.StopLength, ""
	case "tool_calls", "function_call":
		return ai.StopToolUse, ""
	case "":
		compat := struct {
			SupportsFinishReason bool `json:"supportsFinishReason"`
		}{SupportsFinishReason: true}
		if json.Unmarshal(model.Compat, &compat) == nil && !compat.SupportsFinishReason {
			if len(choice.Message.ToolCalls) != 0 {
				return ai.StopToolUse, ""
			}
			return ai.StopStop, ""
		}
		return ai.StopError, "Model completion did not report finish_reason."
	default:
		return ai.StopError, "Provider finish_reason: " + choice.FinishReason
	}
}

func completionEvent(message *ai.AssistantMessage) ai.AssistantMessageEvent {
	if message.StopReason == ai.StopError {
		return ai.AssistantMessageEvent{Type: ai.EventError, Reason: message.StopReason, Error: message}
	}
	return ai.AssistantMessageEvent{Type: ai.EventDone, Reason: message.StopReason, Message: message}
}

type jsonCompletionUsage struct {
	PromptTokens     int64           `json:"prompt_tokens"`
	CompletionTokens int64           `json:"completion_tokens"`
	TotalTokens      int64           `json:"total_tokens"`
	CacheHitTokens   *int64          `json:"prompt_cache_hit_tokens"`
	CachedTokens     *int64          `json:"cached_tokens"`
	ReasoningTokens  int64           `json:"reasoning_tokens"`
	Cost             json.RawMessage `json:"cost"`
	IsBYOK           json.RawMessage `json:"is_byok"`
	CostDetails      json.RawMessage `json:"cost_details"`
	PromptDetails    struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens int64  `json:"cache_write_tokens"`
	} `json:"prompt_tokens_details"`
	CompletionDetails struct {
		ReasoningTokens int64 `json:"reasoning_tokens"`
	} `json:"completion_tokens_details"`
}

func (r jsonCompletionUsage) toUsage(model *ai.Model) ai.Usage {
	count := func(tokens int64) int64 { return min(math.MaxInt32, max(0, tokens)) }
	var cached int64
	for _, value := range []*int64{r.PromptDetails.CachedTokens, r.CacheHitTokens, r.CachedTokens} {
		if value != nil {
			cached = count(*value)
			break
		}
	}
	write := count(r.PromptDetails.CacheWriteTokens)
	input := max(0, count(r.PromptTokens)-cached-write)
	output := count(r.CompletionTokens)
	total := count(r.TotalTokens)
	if total == 0 {
		total = count(input + cached + write + output)
	}
	reasoning := r.CompletionDetails.ReasoningTokens
	if reasoning == 0 {
		reasoning = r.ReasoningTokens
	}
	usage := ai.Usage{
		Input: int(input), CacheRead: int(cached), CacheWrite: int(write), Output: int(output),
		TotalTokens: int(total), Reasoning: int(count(reasoning)),
	}
	ai.CalculateCost(model, &usage)
	if model.Provider == "openrouter" || strings.Contains(model.BaseURL, "openrouter.ai") {
		if cost, ok := r.openRouterCost(); ok {
			usage.Cost.Total, usage.Cost.TotalFromProvider = cost, ai.OptBoolTrue
		}
	}
	return usage
}

func (r jsonCompletionUsage) openRouterCost() (float64, bool) {
	var cost *float64
	var byok *bool
	if json.Unmarshal(r.Cost, &cost) != nil || cost == nil || *cost < 0 ||
		json.Unmarshal(r.IsBYOK, &byok) != nil || byok == nil {
		return 0, false
	}
	if !*byok {
		return *cost, true
	}
	var details struct {
		Inference  *float64 `json:"upstream_inference_cost"`
		Prompt     *float64 `json:"upstream_inference_prompt_cost"`
		Completion *float64 `json:"upstream_inference_completions_cost"`
	}
	if json.Unmarshal(r.CostDetails, &details) != nil {
		return 0, false
	}
	if details.Inference != nil && *details.Inference >= 0 {
		total := *cost + *details.Inference
		return total, !math.IsInf(total, 0)
	}
	if details.Inference == nil && details.Prompt != nil && *details.Prompt >= 0 &&
		details.Completion != nil && *details.Completion >= 0 {
		total := *cost + *details.Prompt + *details.Completion
		return total, !math.IsInf(total, 0)
	}
	return 0, false
}
