package pigo

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/guanshan/pi-go/packages/agent"
	"github.com/guanshan/pi-go/packages/ai"

	"github.com/OXeu/shinona/internal/run"
)

const (
	partLimit     = 48
	partBytes     = 8 << 10
	argumentBytes = 4 << 10
)

type turnParts struct {
	report     func([]run.TurnPart)
	completed  []run.TurnPart
	current    []run.TurnPart
	toolIndex  map[string]int
	finalStart int
	lastReport time.Time
}

func (p *turnParts) snapshot() []run.TurnPart {
	return slices.Concat(p.completed, p.current)
}

func (p *turnParts) event(_ context.Context, event agent.AgentEvent) error {
	switch ev := event.(type) {
	case agent.MessageUpdateEvent:
		if time.Since(p.lastReport) < 150*time.Millisecond {
			return nil
		}
		if message, ok := ai.AsAssistantMessage(ev.Message); ok {
			if p.report != nil {
				p.current = p.messageParts(message, false)
			}
		}
	case agent.MessageEndEvent:
		message, ok := ai.AsAssistantMessage(ev.Message)
		if !ok {
			return nil
		}
		p.finalStart = len(p.completed)
		p.completed = append(p.completed, p.messageParts(message, true)...)
		p.current = nil
	case agent.ToolExecutionStartEvent:
		if index, ok := p.toolIndex[ev.ToolCallID]; ok {
			p.completed[index].Status = string(run.Running)
			p.completed[index].Input = clipPart(string(ev.Args), argumentBytes)
		}
	case agent.ToolExecutionEndEvent:
		if index, ok := p.toolIndex[ev.ToolCallID]; ok {
			state := run.Succeeded
			if ev.IsError {
				state = run.Failed
			}
			if display, ok := ev.Result.Details.(renderedUI); ok && !ev.IsError {
				p.completed[index] = run.TurnPart{Kind: "ui", Text: display.summary, Output: display.spec}
				break
			}
			p.completed[index].Status = string(state)
			p.completed[index].Output = toolOutput(ev.Result.Content)
		}
	default:
		return nil
	}
	if p.report != nil {
		p.report(p.snapshot())
	}
	p.lastReport = time.Now()
	return nil
}

func (p *turnParts) messageParts(message ai.AssistantMessage, complete bool) []run.TurnPart {
	var parts []run.TurnPart
	for _, block := range message.Content {
		if len(p.completed)+len(parts) == partLimit {
			break
		}
		switch block.Type {
		case "text":
			if block.Text != "" {
				parts = append(parts, run.TurnPart{Kind: "text", Text: clipPart(block.Text, partBytes)})
			}
		case "thinking":
			if !block.Redacted && block.Thinking != "" {
				part := run.TurnPart{Kind: "thinking", Text: clipPart(block.Thinking, partBytes)}
				if !complete {
					part.Status = string(run.Running)
					// Use the untruncated UTF-8 text. The endpoint does not report
					// per-block token usage as it streams.
					part.EstimatedTokens = int32((len(block.Thinking) + 3) / 4)
				}
				parts = append(parts, part)
			}
		case "toolCall":
			if block.Name == "" {
				continue
			}
			if complete {
				p.toolIndex[block.ID] = len(p.completed) + len(parts)
			}
			parts = append(parts, run.TurnPart{
				Kind: "tool", Tool: clipPart(block.Name, 256), Input: clipPart(string(block.Arguments), argumentBytes),
				Status: string(run.Queued),
			})
		}
	}
	return parts
}

func toolOutput(blocks []ai.ContentBlock) string {
	var text strings.Builder
	for _, block := range blocks {
		if block.Type != "text" || block.Text == "" {
			continue
		}
		if text.Len() > 0 {
			text.WriteByte('\n')
		}
		remaining := partBytes - text.Len()
		text.WriteString(clipPart(block.Text, remaining))
		if len(block.Text) > remaining {
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
