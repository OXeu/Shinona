package sessiontransfer

import (
	"slices"
	"strings"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
)

func (s *liveConnection) advanceProgress(current run.LiveProgress, offset int) *sessionpb.LiveProgress {
	previous := s.progress
	// The watcher may still have notifications covered by the last sync.
	if offset < previous.FinalizedParts {
		current.Parts = current.Parts[previous.FinalizedParts-offset:]
		offset = previous.FinalizedParts
	}
	delta := progressDelta(previous, current, offset)
	prefix := previous.Parts[:offset-previous.FinalizedParts]
	if current.FinalizedParts == previous.FinalizedParts {
		clear(previous.Parts[len(prefix):])
		current.Parts = append(prefix, current.Parts...)
	} else {
		// Detach from the old backing array so the completed parts can be collected.
		finalizedPrefix := min(current.FinalizedParts, offset) - previous.FinalizedParts
		finalizedTail := max(current.FinalizedParts-offset, 0)
		current.Parts = slices.Concat(prefix[finalizedPrefix:], current.Parts[finalizedTail:])
	}
	s.progress = current
	return delta
}

func progressDelta(previous, current run.LiveProgress, offset int) *sessionpb.LiveProgress {
	partCount := offset + len(current.Parts)
	change := &sessionpb.LiveProgress{TurnId: current.TurnID, PartCount: uint32(partCount)}
	for part, next := range current.Parts {
		index := offset + part
		previousIndex := index - previous.FinalizedParts
		if previousIndex < len(previous.Parts) {
			last := previous.Parts[previousIndex]
			if last == next {
				continue
			}
			if appendOnly(last, next) {
				change.Parts = append(change.Parts, &sessionpb.LivePartChange{
					Index: uint32(index),
					Item: &sessionpb.LivePartChange_Append{Append: &sessionpb.LivePartAppend{
						Text: next.Text[len(last.Text):], Output: next.Output[len(last.Output):],
						EstimatedTokens: next.EstimatedTokens,
					}},
				})
				continue
			}
		}
		change.Parts = append(change.Parts, &sessionpb.LivePartChange{
			Index: uint32(index), Item: &sessionpb.LivePartChange_Replace{Replace: turnPart(next)},
		})
	}
	if !slices.Equal(previous.Steering, current.Steering) {
		change.Steering = &sessionpb.LiveSteering{Messages: steeringMessages(current.Steering)}
	}
	if previous.Context != current.Context {
		change.Context = &sessionpb.ContextUsage{Tokens: current.Context.Tokens, Window: current.Context.Window, Estimated: current.Context.Estimated}
	}
	if len(change.Parts) == 0 && change.Steering == nil && change.Context == nil &&
		previous.FinalizedParts+len(previous.Parts) == partCount {
		return nil
	}
	return change
}

func appendOnly(last, next run.TurnPart) bool {
	return last.Kind == next.Kind && last.Tool == next.Tool && last.Input == next.Input && last.Status == next.Status &&
		strings.HasPrefix(next.Text, last.Text) && strings.HasPrefix(next.Output, last.Output)
}
