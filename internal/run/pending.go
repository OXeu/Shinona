package run

import (
	"context"
	"time"
)

type PendingDraft struct {
	Prompt string
	Images []Image
}

func (s *Service) TakeSteering(owner, sessionID, runID, messageID string) (PendingDraft, error) {
	var draft PendingDraft
	err := s.controls.updateSteering(owner, sessionID, runID, false, func(mailbox *Steering) error {
		return mailbox.take(messageID, func(message SteeringMessage) error {
			draft.Prompt = message.Text
			return nil
		})
	})
	return draft, err
}

func (s *Service) TakeQueued(ctx context.Context, owner, sessionID, turnID string) (PendingDraft, error) {
	draft, err := s.repo.TakeQueued(ctx, owner, sessionID, turnID, false, time.Now().UTC())
	if err == nil {
		s.controls.changed(owner, sessionID)
		s.controls.wakeQueue()
	}
	return draft, err
}

func (s *Service) QueueSteering(ctx context.Context, owner, sessionID, runID, messageID string, options Options) error {
	if !validOptions(options) {
		return ErrOptions
	}
	item, err := s.prepareQueued(ctx, Run{OwnerID: owner, SessionID: sessionID, Payload: Payload{Options: options}})
	if err != nil {
		return err
	}
	err = s.controls.updateSteering(owner, sessionID, runID, true, func(mailbox *Steering) error {
		return mailbox.take(messageID, func(message SteeringMessage) error {
			item.Prompt = message.Text
			_, err := s.repo.Enqueue(ctx, item, "", true)
			return err
		})
	})
	if err == nil {
		s.controls.wakeQueue()
	}
	return err
}

func (s *Service) SteerQueued(ctx context.Context, owner, sessionID, turnID, runID string) error {
	return s.controls.updateSteering(owner, sessionID, runID, true, func(mailbox *Steering) error {
		return mailbox.enqueue(func() (SteeringMessage, error) {
			draft, err := s.repo.TakeQueued(ctx, owner, sessionID, turnID, true, time.Now().UTC())
			return SteeringMessage{ID: turnID, Text: draft.Prompt}, err
		})
	})
}
