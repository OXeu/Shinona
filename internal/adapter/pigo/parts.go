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

// liveParts projects current activity and keeps assistant output visible as tools run.
func liveParts(report func([]run.TurnPart)) func(agent.AgentEvent) {
	var lastReport time.Time
	var output []run.TurnPart
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
		case agent.EvMessageUpdate, agent.EvMessageEnd:
			message := assistantMessage(event.Message)
			if message == nil {
				return
			}
			streaming := event.Type == agent.EvMessageUpdate
			for _, block := range message.Content {
				if part, ok := textPart(block, streaming); ok {
					switch {
					case streaming:
						parts = append(parts, part)
					case part.Kind == "text":
						output = append(output, part)
					}
				}
			}
		case agent.EvToolExecutionStart, agent.EvToolExecutionUpdate:
			part := toolPart(event.ToolName, event.Args, run.Running)
			if result, ok := event.PartialResult.(agent.AgentToolResult); ok {
				part.Output = toolOutput(result.Content)
			}
			parts = []run.TurnPart{part}
		case agent.EvToolExecutionEnd:
			if result, ok := event.Result.(agent.AgentToolResult); ok && !event.IsError {
				if display, ok := result.Details.(renderedUI); ok {
					output = append(output, uiPart(display))
				}
			}
		default:
			return
		}
		report(slices.Concat(output, parts))
		lastReport = time.Now()
	}
}

func transcriptParts(messages []agent.AgentMessage, omitFinalText bool) []run.TurnPart {
	var parts []run.TurnPart
	toolCalls := make(map[string]int)
	for index, entry := range messages {
		if result, ok := entry.(ai.ToolResultMessage); ok {
			if call, found := toolCalls[result.ToolCallID]; found {
				part := &parts[call]
				if result.IsError {
					part.Status = string(run.Failed)
				} else if display, ok := result.Details.(renderedUI); ok {
					*part = uiPart(display)
					continue
				} else {
					part.Status = string(run.Succeeded)
				}
				part.Output = toolOutput(result.Content)
			}
			continue
		}
		message := assistantMessage(entry)
		if message == nil {
			continue
		}
		clear(toolCalls)
		for _, block := range message.Content {
			if call, ok := block.(ai.ToolCall); ok && call.Name != "" {
				toolCalls[call.ID] = len(parts)
				parts = append(parts, toolPart(call.Name, call.Arguments, run.Queued))
				continue
			}
			if part, ok := textPart(block, false); ok && (part.Kind != "text" || !omitFinalText || index != len(messages)-1) {
				parts = append(parts, part)
			}
		}
	}
	return parts
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
	return run.TurnPart{Kind: "ui", Tool: "arrow-js", Text: display.summary, Output: display.source}
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
