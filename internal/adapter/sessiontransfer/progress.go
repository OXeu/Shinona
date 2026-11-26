package sessiontransfer

import (
	"slices"
	"strings"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
)

func progressDelta(previous, current run.LiveProgress, offset int) *sessionpb.LiveProgress {
	partCount := offset + len(current.Parts)
	change := &sessionpb.LiveProgress{TurnId: current.TurnID, PartCount: uint32(partCount)}
	for part, next := range current.Parts {
		index := offset + part
		if index < len(previous.Parts) {
			last := previous.Parts[index]
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
	if len(change.Parts) == 0 && change.Steering == nil && change.Context == nil && len(previous.Parts) == partCount {
		return nil
	}
	return change
}

func appendOnly(last, next run.TurnPart) bool {
	return last.Kind == next.Kind && last.Tool == next.Tool && last.Input == next.Input && last.Status == next.Status &&
		strings.HasPrefix(next.Text, last.Text) && strings.HasPrefix(next.Output, last.Output)
}
