package rest

import (
	"context"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

type pendingDraftResponse struct {
	Prompt string      `json:"prompt"`
	Images []run.Image `json:"images"`
}

func (r *handler) takeSteering(ctx context.Context, args struct {
	SessionID  string `uri:"id" json:"-"`
	RunID      string `uri:"turnId" json:"-"`
	SteeringID string `uri:"steeringId" json:"-"`
}) (pendingDraftResponse, error) {
	draft, err := r.service.TakeSteering(auth.CurrentUser(ctx).ID, args.SessionID, args.RunID, args.SteeringID)
	return pendingDraftResponse{Prompt: draft.Prompt, Images: []run.Image{}}, r.apiError(err)
}

func (r *handler) takeQueued(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	TurnID    string `uri:"turnId" json:"-"`
}) (pendingDraftResponse, error) {
	draft, err := r.service.TakeQueued(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.TurnID)
	return pendingDraftResponse{Prompt: draft.Prompt, Images: append([]run.Image{}, draft.Images...)}, r.apiError(err)
}

func (r *handler) queueSteering(ctx context.Context, args struct {
	SessionID  string `uri:"id" json:"-"`
	RunID      string `uri:"turnId" json:"-"`
	SteeringID string `uri:"steeringId" json:"-"`
	Options    *payloadInput
}) (bool, error) {
	err := r.service.QueueSteering(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.RunID, args.SteeringID, args.Options.payload().Options)
	return err == nil, r.apiError(err)
}

func (r *handler) steerQueued(ctx context.Context, args struct {
	SessionID string `uri:"id" json:"-"`
	TurnID    string `uri:"turnId" json:"-"`
	RunID     string
}) (bool, error) {
	err := r.service.SteerQueued(ctx, auth.CurrentUser(ctx).ID, args.SessionID, args.TurnID, args.RunID)
	return err == nil, r.apiError(err)
}
