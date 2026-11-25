package sessiontransfer

import (
	"slices"
	"time"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
)

func summary(session run.Session) *sessionpb.SessionSummary {
	return &sessionpb.SessionSummary{
		Id: session.ID, ProjectId: optionalText(session.ProjectID), Title: session.Title,
		DefaultTitle: session.TurnCount == 0 && !session.CustomTitle,
		TurnCount:    int32(session.TurnCount), UpdatedAt: session.UpdatedAt.Format(time.RFC3339Nano),
		Pinned: session.Pinned, Active: session.Active, Unread: session.Unread,
	}
}

func sendTranscript(transcript *run.Transcript, settings run.Settings, send func(*sessionpb.TranscriptChunk) error) error {
	header := &sessionpb.TranscriptHeader{SandboxSettings: sandboxSettings(settings)}
	if transcript != nil {
		header.Session = summary(transcript.Session)
		header.LastOptions = runOptions(transcript.LastOptions())
		header.NextBeforeSequence = int32(transcript.NextBeforeSequence)
	}
	if err := send(&sessionpb.TranscriptChunk{Item: &sessionpb.TranscriptChunk_Header{Header: header}}); err != nil {
		return err
	}
	if transcript != nil {
		for batch := range slices.Chunk(transcript.Turns, turnBatchSize) {
			turns := make([]*sessionpb.Turn, len(batch))
			for i, item := range batch {
				turns[i] = turn(item)
			}
			if err := send(&sessionpb.TranscriptChunk{
				Item: &sessionpb.TranscriptChunk_Turns{Turns: &sessionpb.TranscriptTurns{Turns: turns}},
			}); err != nil {
				return err
			}
		}
	}
	return send(&sessionpb.TranscriptChunk{Item: &sessionpb.TranscriptChunk_Done{Done: true}})
}

func runOptions(options *run.Options) *sessionpb.RunOptions {
	if options == nil {
		return nil
	}
	return &sessionpb.RunOptions{Model: options.Model, Permission: options.Permission,
		Thinking: options.Thinking, McpServers: options.SelectedMCPServers(), ArrowJs: options.ArrowJS}
}

func turn(item run.Run) *sessionpb.Turn {
	result := &sessionpb.Turn{
		Id: item.ID, Sequence: int32(item.Sequence), Prompt: item.Prompt,
		Status: string(item.Status), Policy: optionalText(string(item.Policy)), Output: item.Output,
		CreatedAt: item.CreatedAt.Format(time.RFC3339Nano), UpdatedAt: item.UpdatedAt.Format(time.RFC3339Nano),
		Model: item.Payload.Options.Model, ContextTokens: int32(item.ContextTokens), ContextWindow: int32(item.ContextWindow),
		LiveContext: contextUsage(item.LiveContext),
		Attachments: make([]string, len(item.Payload.Images)),
		Steering:    steeringMessages(item.Steering),
		Parts:       make([]*sessionpb.TurnPart, len(item.Parts)),
	}
	if item.StartedAt != nil {
		result.StartedAt = new(item.StartedAt.Format(time.RFC3339Nano))
	}
	if usage := item.TokenUsage; usage != nil {
		result.TokenUsage = &sessionpb.TokenUsage{Input: usage.Input, Cached: usage.Cached, Output: usage.Output}
	}
	for i, image := range item.Payload.Images {
		result.Attachments[i] = image.Name
	}
	for i, part := range item.Parts {
		result.Parts[i] = turnPart(part)
	}
	return result
}

func contextUsage(usage run.ContextUsage) *sessionpb.ContextUsage {
	if usage.Tokens == 0 {
		return nil
	}
	return &sessionpb.ContextUsage{Tokens: usage.Tokens, Window: usage.Window, Estimated: usage.Estimated}
}

func sandboxSettings(settings run.Settings) *sessionpb.SandboxSettings {
	result := &sessionpb.SandboxSettings{Available: make([]string, len(settings.Available)), Selected: optionalText(string(settings.Selected))}
	for i, policy := range settings.Available {
		result.Available[i] = string(policy)
	}
	return result
}

func steeringMessages(messages []run.SteeringMessage) []*sessionpb.SteeringMessage {
	result := make([]*sessionpb.SteeringMessage, len(messages))
	for i, message := range messages {
		result[i] = &sessionpb.SteeringMessage{Text: message.Text, Delivered: message.Delivered}
	}
	return result
}

func turnPart(part run.TurnPart) *sessionpb.TurnPart {
	return &sessionpb.TurnPart{
		Kind: part.Kind, Text: part.Text, Tool: part.Tool, Input: part.Input, Output: part.Output,
		Status: part.Status, EstimatedTokens: part.EstimatedTokens,
	}
}

func optionalText(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
