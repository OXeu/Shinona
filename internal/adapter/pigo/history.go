package pigo

import (
	"encoding/json"
	"slices"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
)

func encodeMessages(messages []agent.AgentMessage, compacted bool) ([]byte, error) {
	if len(messages) == 0 {
		return nil, nil
	}
	// The repository already stores the initial user message and its attachments.
	replay := make([]agent.AgentMessage, 0, len(messages))
	promptSeen := compacted
	for _, entry := range messages {
		switch entry.MessageRole() {
		case ai.RoleSystem:
			continue
		case ai.RoleUser:
			if !promptSeen {
				promptSeen = true
				continue
			}
			replay = append(replay, entry)
		case ai.RoleAssistant:
			message := assistantMessage(entry)
			if message.StopReason == ai.StopError || message.StopReason == ai.StopAborted {
				continue
			}
			copy := *message
			copy.Diagnostics = nil
			copy.ErrorMessage = ""
			replay = append(replay, copy)
		case ai.RoleToolResult:
			if message, ok := entry.(ai.ToolResultMessage); ok {
				message.Details = nil
				replay = append(replay, message)
			}
		default:
			replay = append(replay, entry)
		}
	}
	return json.Marshal(replay)
}

func conversationHistory(exchanges []run.Exchange, fallbackModel ai.Model) ([]agent.AgentMessage, error) {
	history := make([]agent.AgentMessage, 0, len(exchanges)*2)
	for i := len(exchanges) - 1; i >= 0; i-- {
		if len(exchanges[i].CompactedContext) > 0 {
			var err error
			history, err = appendMessages(history, exchanges[i].CompactedContext)
			if err != nil {
				return nil, err
			}
			exchanges = exchanges[i+1:]
			break
		}
	}
	for _, exchange := range exchanges {
		if len(exchange.Images) > 0 {
			content := ai.ContentList{ai.TextContent{Text: exchange.Prompt}}
			for _, image := range imageBlocks(exchange.Images) {
				content = append(content, image)
			}
			history = append(history, ai.UserMessage{Content: content})
		} else if exchange.Prompt != "" {
			history = append(history, ai.NewUserText(exchange.Prompt, 0))
		}
		if len(exchange.AgentMessages) > 0 {
			var err error
			history, err = appendMessages(history, exchange.AgentMessages)
			if err != nil {
				return nil, err
			}
			continue
		}
		for _, steer := range exchange.Steering {
			if steer.Delivered {
				history = append(history, ai.NewUserText(steer.Text, 0))
			}
		}
		if exchange.Output != "" {
			history = append(history, ai.AssistantMessage{
				Api: fallbackModel.Api, Provider: fallbackModel.Provider, Model: fallbackModel.ID, StopReason: ai.StopStop,
				Content: ai.ContentList{ai.TextContent{Text: exchange.Output}},
			})
		}
	}
	return history, nil
}

func appendMessages(history []agent.AgentMessage, data []byte) ([]agent.AgentMessage, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		message, err := restoreMessage(entry)
		if err != nil {
			return nil, err
		}
		history = append(history, message)
	}
	return history, nil
}

func assistantMessage(message ai.Message) *ai.AssistantMessage {
	switch value := message.(type) {
	case ai.AssistantMessage:
		return &value
	case *ai.AssistantMessage:
		return value
	default:
		return nil
	}
}

func restoreMessage(data json.RawMessage) (ai.Message, error) {
	message, err := ai.UnmarshalMessage(data)
	if err != nil {
		return nil, err
	}
	assistant := assistantMessage(message)
	if assistant == nil || !slices.ContainsFunc(assistant.Content, unsignedThinking) {
		return message, nil
	}
	// Releases before the Pi migration stored provider data under signature.
	// Honor that field only on reasoning blocks that lack thinkingSignature.
	var legacy struct {
		Content []struct{ Signature string }
	}
	if err := json.Unmarshal(data, &legacy); err != nil {
		return nil, err
	}
	for i, block := range assistant.Content {
		if i < len(legacy.Content) && unsignedThinking(block) && legacy.Content[i].Signature != "" {
			thinking := block.(ai.ThinkingContent)
			thinking.ThinkingSignature = legacy.Content[i].Signature
			assistant.Content[i] = thinking
		}
	}
	return assistant, nil
}

func unsignedThinking(block ai.Content) bool {
	thinking, ok := block.(ai.ThinkingContent)
	return ok && thinking.ThinkingSignature == ""
}
