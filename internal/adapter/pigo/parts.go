package pigo

import (
	"encoding/json"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/sky-valley/pi/agent"
	"github.com/sky-valley/pi/ai"

	"github.com/OXeu/shinona/internal/run"
)

const (
	partBytes     = 8 << 10
	argumentBytes = 4 << 10
)

type partProjection struct {
	parts     []run.TurnPart
	toolCalls map[string]int
}

// liveParts adds finalized Pi messages in order and overlays the streaming response.
func liveParts(report func([]run.TurnPart)) func(agent.AgentEvent) {
	var lastReport time.Time
	view := partProjection{toolCalls: make(map[string]int)}
	return func(event agent.AgentEvent) {
		if report == nil {
			return
		}
		var parts []run.TurnPart
		switch event.Type {
		case agent.EvMessageUpdate, agent.EvToolExecutionUpdate:
			if time.Since(lastReport) < 150*time.Millisecond {
				return
			}
		}
		switch event.Type {
		case agent.EvMessageUpdate:
			message := assistantMessage(event.Message)
			if message == nil {
				return
			}
			for _, block := range message.Content {
				if part, ok := textPart(block, true); ok {
					parts = append(parts, part)
				}
			}
		case agent.EvMessageEnd:
			if !view.addMessage(event.Message, false) {
				return
			}
		case agent.EvToolExecutionStart, agent.EvToolExecutionUpdate:
			index, ok := view.toolCalls[event.ToolCallID]
			if !ok {
				return
			}
			part := &view.parts[index]
			part.Status = string(run.Running)
			if result, ok := event.PartialResult.(agent.AgentToolResult); ok {
				part.Output = toolOutput(result.Content)
			}
		default:
			return
		}
		report(slices.Concat(view.parts, parts))
		lastReport = time.Now()
	}
}

func transcriptParts(messages []agent.AgentMessage, omitFinalText bool) []run.TurnPart {
	view := partProjection{toolCalls: make(map[string]int)}
	for index, entry := range messages {
		view.addMessage(entry, omitFinalText && index == len(messages)-1)
	}
	return view.parts
}

func (p *partProjection) addMessage(entry agent.AgentMessage, omitText bool) bool {
	if result, ok := entry.(ai.ToolResultMessage); ok {
		index, found := p.toolCalls[result.ToolCallID]
		if !found {
			return false
		}
		part := &p.parts[index]
		if result.IsError {
			part.Status = string(run.Failed)
		} else if display, ok := result.Details.(renderedUI); ok {
			*part = uiPart(display)
			return true
		} else {
			part.Status = string(run.Succeeded)
		}
		part.Output = toolOutput(result.Content)
		return true
	}
	message := assistantMessage(entry)
	if message == nil {
		return false
	}
	clear(p.toolCalls)
	for _, block := range message.Content {
		if call, ok := block.(ai.ToolCall); ok && call.Name != "" {
			p.toolCalls[call.ID] = len(p.parts)
			p.parts = append(p.parts, toolPart(call.Name, call.Arguments, run.Queued))
			continue
		}
		if part, ok := textPart(block, false); ok && (part.Kind != "text" || !omitText) {
			p.parts = append(p.parts, part)
		}
	}
	return true
}

func textPart(block ai.Content, streaming bool) (run.TurnPart, bool) {
	switch block := block.(type) {
	case ai.TextContent:
		if block.Text != "" {
			return run.TurnPart{Kind: "text", Text: clipPart(block.Text, partBytes)}, true
		}
	case ai.ThinkingContent:
		if !block.Redacted && block.Thinking != "" {
			part := run.TurnPart{Kind: "thinking", Text: clipPart(block.Thinking, partBytes)}
			if streaming {
				part.Status = string(run.Running)
				// Use the untruncated UTF-8 text. The endpoint does not report
				// per-block token usage as it streams.
				part.EstimatedTokens = int32((len(block.Thinking) + 3) / 4)
			}
			return part, true
		}
	}
	return run.TurnPart{}, false
}

func uiPart(display renderedUI) run.TurnPart {
	return run.TurnPart{Kind: "ui", Tool: arrowView.Format, Text: display.summary, Output: display.source}
}

func toolPart(name string, args map[string]any, status run.Status) run.TurnPart {
	return run.TurnPart{Kind: "tool", Tool: clipPart(name, 256), Input: argumentPreview(args), Status: string(status)}
}

func argumentPreview(args map[string]any) string {
	raw, _ := json.Marshal(args)
	return clipPart(string(raw), argumentBytes)
}

func toolOutput(blocks ai.ContentList) string {
	var text strings.Builder
	for _, block := range blocks {
		item, ok := block.(ai.TextContent)
		if !ok || item.Text == "" {
			continue
		}
		if text.Len() >= partBytes-1 {
			break
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		remaining := partBytes - text.Len()
		text.WriteString(clipPart(item.Text, remaining))
		if len(item.Text) > remaining {
			break
		}
	}
	return text.String()
}

func clipPart(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	for limit > 0 && !utf8.RuneStart(text[limit]) {
		limit--
	}
	return text[:limit] + "…"
}
