package run

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/keylock"
	"github.com/OXeu/shinona/internal/sandbox"
)

type Service struct {
	stores         ServiceStores
	resolve        ResolveTarget
	projectTargets ListProjectTargets
	newID          func() string
	controls       *Controls
	projectChanges keylock.Locks[sandbox.ProjectKey]
	terminals      terminalRegistry
}

type Settings struct {
	Available []sandbox.Policy
	Selected  sandbox.Policy
}

func NewService(stores ServiceStores, resolve ResolveTarget, newID func() string, controls *Controls, projectTargets ListProjectTargets) *Service {
	return &Service{stores: stores, resolve: resolve, newID: newID, controls: controls, projectTargets: projectTargets}
}

func (s *Service) CreateSession(ctx context.Context, owner, projectID string) (Session, error) {
	now := time.Now().UTC()
	session := Session{ID: s.newID(), OwnerID: owner, ProjectID: projectID, Title: DefaultSessionTitle, CreatedAt: now, UpdatedAt: now}
	if err := s.stores.Sessions.CreateSession(ctx, session); err != nil {
		return Session{}, err
	}
	return session, nil
}

func (s *Service) Submit(ctx context.Context, owner, sessionID, prompt string, payload Payload, queue bool) (Run, error) {
	prompt, err := validatePrompt(prompt, payload.Images)
	if err != nil {
		return Run{}, err
	}
	if !validOptions(payload.Options) {
		return Run{}, ErrOptions
	}

	title := []rune(strings.Join(strings.Fields(prompt), " "))
	if len(title) > 64 {
		title = append(title[:63], '…')
	}
	if len(title) == 0 {
		title = []rune(payload.Images[0].Name)
		if len(title) > 64 {
			title = append(title[:63], '…')
		}
	}
	return s.enqueue(ctx, Run{OwnerID: owner, SessionID: sessionID, Prompt: prompt, Payload: payload}, string(title), queue, nil)
}

func (s *Service) RetryTurn(ctx context.Context, owner, sessionID, turnID string, retry RetryOptions) (Run, error) {
	if turnID == "" {
		return Run{}, ErrRetryTurn
	}
	retry.Model = strings.TrimSpace(retry.Model)
	if !validOptions(retry.Apply(Options{})) {
		return Run{}, ErrOptions
	}
	return s.enqueue(ctx, Run{OwnerID: owner, SessionID: sessionID, RetryOf: turnID}, "", false, &retry)
}

func (s *Service) enqueue(ctx context.Context, item Run, firstTitle string, queue bool, retry *RetryOptions) (Run, error) {
	item, err := s.prepareQueued(ctx, item)
	if err != nil {
		return Run{}, err
	}
	var turn Run
	if retry == nil {
		turn, err = s.stores.Turns.Enqueue(ctx, item, firstTitle, queue)
	} else {
		turn, err = s.stores.Turns.EnqueueRetry(ctx, item, *retry)
	}
	if err == nil {
		s.controls.changed(item.OwnerID, item.SessionID)
		s.controls.wakeQueue()
	}
	return turn, err
}

func (s *Service) prepareQueued(ctx context.Context, item Run) (Run, error) {
	project, err := s.stores.Sessions.SessionProject(ctx, item.OwnerID, item.SessionID)
	if err != nil {
		return Run{}, err
	}
	spec := sandbox.Spec{OwnerID: item.OwnerID, SessionID: item.SessionID, ProjectID: project, Policy: sandbox.Shared}
	_, target, err := projectTarget(ctx, s.stores.Projects, s.resolve, spec, "", false)
	if err != nil {
		return Run{}, err
	}
	settings, err := s.settingsFor(ctx, item.OwnerID, target)
	if err != nil {
		return Run{}, err
	}
	if settings.Selected == "" {
		return Run{}, ErrPolicy
	}
	if _, err := target.ResolveAgent(ctx, item.OwnerID); err != nil {
		return Run{}, err
	}
	now := time.Now().UTC()
	item.ID, item.Backend, item.Policy = s.newID(), target.Name, settings.Selected
	item.Status, item.CreatedAt, item.UpdatedAt = Queued, now, now
	return item, nil
}

func (s *Service) Steer(owner, sessionID, id, prompt string) error {
	prompt, err := validatePrompt(prompt, nil)
	if err != nil {
		return err
	}
	return s.controls.updateSteering(owner, sessionID, id, false, func(mailbox *Steering) error {
		return mailbox.enqueue(func() (SteeringMessage, error) {
			return SteeringMessage{ID: s.newID(), Text: prompt}, nil
		})
	})
}

func (s *Service) Stop(ctx context.Context, owner, sessionID, id string) error {
	return s.controls.stop(ctx, s.stores.Turns, owner, sessionID, id)
}

// A nil project ID includes all projects. An empty project ID selects the default project.
func (s *Service) RecentSessions(ctx context.Context, owner string, projectID *string) ([]Session, error) {
	return s.stores.Sessions.RecentSessions(ctx, owner, projectID, recentSessionLimit)
}

func (s *Service) SessionListRevision(ctx context.Context, owner string) (int64, error) {
	return s.stores.Sessions.SessionListRevision(ctx, owner)
}

func (s *Service) Transcript(ctx context.Context, owner, id string) (*Transcript, error) {
	return s.TranscriptPage(ctx, owner, id, 0)
}

func (s *Service) FullTranscript(ctx context.Context, owner, id string) (*Transcript, error) {
	// Legacy full WebSocket subscriptions emit one frame. Larger histories
	// use paged subscriptions or the HTTP streaming transcript.
	transcript, err := s.stores.Transcripts.Transcript(ctx, owner, id, MaxLiveTranscriptTurns+1)
	if transcript != nil && len(transcript.Turns) > MaxLiveTranscriptTurns {
		return nil, ErrTranscriptExportLimit
	}
	return s.withTranscriptProgress(transcript, err)
}

func (s *Service) TranscriptPage(ctx context.Context, owner, id string, beforeSequence int) (*Transcript, error) {
	return s.withTranscriptProgress(s.stores.Transcripts.TranscriptPage(ctx, owner, id, beforeSequence))
}

func (s *Service) StreamTranscript(ctx context.Context, owner, id string, write func(TranscriptStream) error) error {
	progress := s.controls.progress(sessionKey{owner, id})
	return s.stores.Transcripts.StreamTranscript(ctx, owner, id, func(stream TranscriptStream) error {
		if stream.Next != nil && progress.TurnID != "" {
			next := stream.Next
			stream.Next = func() ([]Run, error) {
				batch, err := next()
				if err == nil {
					for i := range batch {
						turn := &batch[i]
						if turn.Status == Running && turn.ID == progress.TurnID {
							turn.Parts, turn.Steering, turn.LiveContext = progress.Parts, progress.Steering, progress.Context
						}
					}
				}
				return batch, err
			}
		}
		return write(stream)
	})
}
func (s *Service) withTranscriptProgress(transcript *Transcript, err error) (*Transcript, error) {
	if err != nil || transcript == nil {
		return transcript, err
	}
	s.addTranscriptProgress(transcript.Turns)
	return transcript, nil
}

func (s *Service) addTranscriptProgress(turns []Run) {
	for i := range turns {
		turn := &turns[i]
		if turn.Status == Running {
			s.controls.addProgress(turn)
		}
	}
}

func (s *Service) Attachment(ctx context.Context, owner, sessionID, turnID string, index int) (*Image, error) {
	if owner == "" || index < 0 || index >= MaxImages {
		return nil, nil
	}
	return s.stores.Transcripts.Attachment(ctx, owner, sessionID, turnID, index)
}

func (s *Service) AttachmentTurnVisible(ctx context.Context, owner, sessionID, turnID string) (bool, error) {
	if owner == "" {
		return false, nil
	}
	return s.stores.Transcripts.AttachmentTurnVisible(ctx, owner, sessionID, turnID)
}

func (s *Service) UpdateSession(ctx context.Context, owner, id string, changes SessionChanges) (Session, error) {
	if changes.Title != nil {
		title := strings.Join(strings.Fields(*changes.Title), " ")
		if title == "" || utf8.RuneCountInString(title) > 80 {
			return Session{}, ErrInvalidTitle
		}
		changes.Title = &title
	}
	updated, err := s.stores.Sessions.UpdateSession(ctx, owner, id, changes)
	if err == nil {
		s.controls.changed(owner, id)
	}
	return updated, err
}

func (s *Service) ArchiveSession(ctx context.Context, owner, id string) error {
	err := s.stores.Sessions.ArchiveSession(ctx, owner, id, time.Now().UTC())
	if err == nil {
		s.terminals.closeSession(sessionKey{owner, id})
		s.controls.changed(owner, id)
	}
	return err
}

func (s *Service) MarkSessionRead(ctx context.Context, owner, id string, through int, throughID string) error {
	err := s.stores.Sessions.MarkSessionRead(ctx, owner, id, through, throughID)
	if err == nil {
		s.controls.changed(owner, id)
	}
	return err
}

func (s *Service) RevertTurn(ctx context.Context, owner, sessionID, turnID, expectedHeadID string) (RevertedTurn, error) {
	result, err := s.stores.Turns.RevertTurn(ctx, owner, sessionID, turnID, expectedHeadID, time.Now().UTC())
	if err == nil {
		s.controls.changed(owner, sessionID)
	}
	return result, err
}

func (s *Service) Settings(ctx context.Context, owner string) (Settings, error) {
	target, err := s.resolve(ctx, owner, "")
	if err != nil {
		return Settings{}, err
	}
	return s.settingsFor(ctx, owner, target)
}

func (s *Service) ComposerDefaults(ctx context.Context, owner string) (*ComposerDefaults, error) {
	return s.stores.Preferences.ComposerDefaults(ctx, owner)
}

func (s *Service) SetComposerDefaults(ctx context.Context, owner string, defaults ComposerDefaults) error {
	options := Options{Model: defaults.Model, Permission: defaults.Permission, Thinking: defaults.Thinking}
	if defaults.Permission == "" || defaults.Thinking == "" || !validOptions(options) {
		return ErrOptions
	}
	return s.stores.Preferences.SaveComposerDefaults(ctx, owner, defaults)
}

func (s *Service) settingsFor(ctx context.Context, owner string, target Target) (Settings, error) {
	available := target.policies(owner)
	if len(available) == 0 {
		return Settings{Available: available}, nil
	}
	saved, err := s.stores.Preferences.Policy(ctx, owner)
	if err != nil {
		return Settings{}, err
	}
	if !slices.Contains(available, saved) {
		saved = available[0]
	}
	return Settings{Available: available, Selected: saved}, nil
}

func (s *Service) SetPolicy(ctx context.Context, owner string, choice sandbox.Policy) (Settings, error) {
	target, err := s.resolve(ctx, owner, "")
	if err != nil {
		return Settings{}, err
	}
	available := target.policies(owner)
	if !slices.Contains(available, choice) {
		return Settings{}, ErrPolicy
	}
	if err := s.stores.Preferences.SetPolicy(ctx, owner, choice); err != nil {
		return Settings{}, err
	}
	s.controls.changedAccount(owner)
	return Settings{Available: available, Selected: choice}, nil
}
