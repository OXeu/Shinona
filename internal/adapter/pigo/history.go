package pigo

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"
	"github.com/sky-valley/pi/coding"

	"github.com/OXeu/shinona/internal/run"
)

type sessionHistory struct {
	messages   []agent.AgentMessage
	checkpoint int // Number of messages recorded in the saved context snapshot.
}

func (h sessionHistory) load(session *coding.Session, prompt ai.SystemMessage) int {
	messages := slices.Concat([]agent.AgentMessage{prompt}, h.messages)
	if h.checkpoint == 0 {
		session.LoadHistory(messages)
		return len(messages)
	}
	summary, ok := savedPiSummary(h.messages[0])
	if !ok {
		session.LoadHistory(messages)
		return len(messages)
	}
	// Restore the boundary through Pi so later compaction updates its prior
	// summary. Rebuild the prompt with the current permissions and tools.
	const checkpointID = "checkpoint"
	sources := make([]coding.SessionEntry, len(h.messages))
	entries := make([]coding.ProjectedSessionEntry, len(h.messages))
	sources[0] = coding.SessionEntry{ID: checkpointID, Type: "compaction", Summary: summary, SystemMessage: &prompt}
	entries[0] = coding.ProjectedSessionEntry{SourceEntry: &sources[0], Messages: messages[:2]}
	for i := 1; i < len(h.messages); i++ {
		sources[i] = coding.SessionEntry{Type: "message", Message: h.messages[i]}
		if i == h.checkpoint {
			sources[i].ParentID = checkpointID
		}
		entries[i] = coding.ProjectedSessionEntry{SourceEntry: &sources[i], Messages: messages[i+1 : i+2]}
	}
	session.LoadBranch(coding.BranchProjection{BranchContext: coding.BranchContext{Messages: messages}, Entries: entries})
	return len(messages)
}

// Stored contexts include Pi's rendered summary as their first user message.
// Read that format only from a snapshot, never from an ordinary turn prompt.
func savedPiSummary(message agent.AgentMessage) (string, bool) {
	user, ok := message.(ai.UserMessage)
	if !ok || len(user.Content) != 1 {
		return "", false
	}
	block, ok := user.Content[0].(ai.TextContent)
	if !ok {
		return "", false
	}
	summary, ok := strings.CutPrefix(block.Text, "The conversation history before this point was compacted into the following summary:\n\n<summary>\n")
	if !ok {
		return "", false
	}
	return strings.CutSuffix(summary, "\n</summary>")
}

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

func conversationHistory(exchanges []run.Exchange, fallbackModel ai.Model) (sessionHistory, error) {
	history := make([]agent.AgentMessage, 0, len(exchanges)*2)
	checkpoint := 0
	for i := len(exchanges) - 1; i >= 0; i-- {
		if len(exchanges[i].CompactedContext) > 0 {
			var err error
			history, err = appendMessages(history, exchanges[i].CompactedContext)
			if err != nil {
				return sessionHistory{}, err
			}
			checkpoint = len(history)
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
				return sessionHistory{}, err
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
	return sessionHistory{messages: history, checkpoint: checkpoint}, nil
}

func appendMessages(history []agent.AgentMessage, data []byte) ([]agent.AgentMessage, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	for _, entry := range entries {
		message, err := ai.UnmarshalMessage(entry)
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
