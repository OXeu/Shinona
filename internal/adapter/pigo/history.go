package pigo

import (
	"encoding/json"
	"slices"

	"github.com/guanshan/pi-go/packages/agent"
	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/run"
)

func encodeTurnMessages(messages []agent.AgentMessage) ([]byte, error) {
	if len(messages) == 0 {
		return nil, nil
	}
	// RunAgentLoop returns the initial prompt followed by the messages it added.
	// The repository already stores the prompt and its attachments.
	replay := make([]agent.AgentMessage, 0, len(messages)-1)
	for _, entry := range messages[1:] {
		switch message := entry.(type) {
		case ai.AssistantMessage:
			if message.StopReason == "error" || message.StopReason == "aborted" {
				continue
			}
			message.Diagnostics = nil
			message.ErrorMessage = ""
			replay = append(replay, message)
		case ai.ToolResultMessage:
			message.Details = nil
			replay = append(replay, message)
		default:
			replay = append(replay, entry)
		}
	}
	return json.Marshal(replay)
}

func conversationHistory(exchanges []run.Exchange, model ai.Model) ([]agent.AgentMessage, error) {
	supportsImages := slices.Contains(model.Input, "image")
	history := make([]agent.AgentMessage, 0, len(exchanges)*2)
	for _, exchange := range exchanges {
		prompt := exchange.Prompt
		var blocks []ai.ContentBlock
		if supportsImages {
			blocks = imageBlocks(exchange.Images)
		} else if prompt == "" && len(exchange.Images) > 0 && len(exchange.AgentMessages) > 0 {
			prompt = "(image omitted: model does not support images)"
		}
		if prompt != "" || len(blocks) > 0 {
			history = append(history, ai.NewUserMessage(prompt, blocks))
		}
		if len(exchange.AgentMessages) > 0 {
			var entries []json.RawMessage
			if err := json.Unmarshal(exchange.AgentMessages, &entries); err != nil {
				return nil, err
			}
			for _, entry := range entries {
				message, err := ai.UnmarshalMessageJSON(entry)
				if err != nil {
					return nil, err
				}
				history = append(history, message)
			}
			continue
		}
		for _, steer := range exchange.Steering {
			if steer.Delivered {
				history = append(history, ai.NewUserMessage(steer.Text, nil))
			}
		}
		if exchange.Output != "" {
			history = append(history, ai.NewAssistantMessageForModel(model, ai.TextBlocks(exchange.Output), ai.Usage{}, "stop"))
		}
	}
	return history, nil
}
