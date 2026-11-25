package run

import (
	"context"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/sandbox"
)

type Service struct {
	repo      Repository
	resolve   ResolveTarget
	newID     func() string
	controls  *Controls
	terminals terminalRegistry
}

type Settings struct {
	Available []sandbox.Policy
	Selected  sandbox.Policy
}

func NewService(repo Repository, resolve ResolveTarget, newID func() string, controls *Controls) *Service {
	return &Service{repo: repo, resolve: resolve, newID: newID, controls: controls}
}

func (s *Service) CreateSession(ctx context.Context, owner, projectID string) (Session, error) {
	now := time.Now().UTC()
	session := Session{ID: s.newID(), OwnerID: owner, ProjectID: projectID, Title: DefaultSessionTitle, CreatedAt: now, UpdatedAt: now}
	if err := s.repo.CreateSession(ctx, session); err != nil {
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
	options := Options{Model: retry.Model}
	if retry.Selection != nil {
		options = *retry.Selection
	}
	if !validOptions(options) {
		return Run{}, ErrOptions
	}
	return s.enqueue(ctx, Run{OwnerID: owner, SessionID: sessionID, RetryOf: turnID}, "", false, &retry)
}

func (s *Service) enqueue(ctx context.Context, item Run, firstTitle string, queue bool, retry *RetryOptions) (Run, error) {
	target := s.resolve("")
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
	var turn Run
	if retry == nil {
		turn, err = s.repo.Enqueue(ctx, item, firstTitle, queue)
	} else {
		turn, err = s.repo.EnqueueRetry(ctx, item, *retry)
	}
	if err == nil {
		s.controls.changed(item.OwnerID, item.SessionID)
	}
	return turn, err
}

func (s *Service) Steer(owner, sessionID, id, prompt string) error {
	prompt, err := validatePrompt(prompt, nil)
	if err != nil {
		return err
	}
	return s.controls.steer(owner, sessionID, id, prompt)
}

func (s *Service) Stop(ctx context.Context, owner, sessionID, id string) error {
	return s.controls.stop(ctx, s.repo, owner, sessionID, id)
}

// A nil project ID includes all projects. An empty project ID selects the default project.
func (s *Service) RecentSessions(ctx context.Context, owner string, projectID *string) ([]Session, error) {
	return s.repo.RecentSessions(ctx, owner, projectID, recentSessionLimit)
}

func (s *Service) Transcript(ctx context.Context, owner, id string) (*Transcript, error) {
	return s.withTranscriptProgress(s.repo.Transcript(ctx, owner, id, 0))
}

func (s *Service) TranscriptPage(ctx context.Context, owner, id string, beforeSequence int) (*Transcript, error) {
	return s.withTranscriptProgress(s.repo.TranscriptPage(ctx, owner, id, beforeSequence))
}

func (s *Service) withTranscriptProgress(transcript *Transcript, err error) (*Transcript, error) {
	if err != nil || transcript == nil {
		return transcript, err
	}
	for i := range transcript.Turns {
		turn := &transcript.Turns[i]
		if turn.Status == Running {
			s.controls.addProgress(turn)
		}
	}
	return transcript, nil
}

func (s *Service) Attachment(ctx context.Context, owner, sessionID, turnID string, index int) (*Image, error) {
	if owner == "" || index < 0 || index >= MaxImages {
		return nil, nil
	}
	return s.repo.Attachment(ctx, owner, sessionID, turnID, index)
}

func (s *Service) UpdateSession(ctx context.Context, owner, id string, changes SessionChanges) (Session, error) {
	if changes.Title != nil {
		title := strings.Join(strings.Fields(*changes.Title), " ")
		if title == "" || utf8.RuneCountInString(title) > 80 {
			return Session{}, ErrInvalidTitle
		}
		changes.Title = &title
	}
	updated, err := s.repo.UpdateSession(ctx, owner, id, changes)
	if err == nil {
		s.controls.changed(owner, id)
	}
	return updated, err
}

func (s *Service) ArchiveSession(ctx context.Context, owner, id string) error {
	err := s.repo.ArchiveSession(ctx, owner, id, time.Now().UTC())
	if err == nil {
		s.terminals.closeSession(sessionKey{owner, id})
		s.controls.changed(owner, id)
	}
	return err
}

func (s *Service) MarkSessionRead(ctx context.Context, owner, id string, through int, throughID string) error {
	err := s.repo.MarkSessionRead(ctx, owner, id, through, throughID)
	if err == nil {
		s.controls.changed(owner, id)
	}
	return err
}

func (s *Service) RevertTurn(ctx context.Context, owner, sessionID, turnID, expectedHeadID string) (RevertedTurn, error) {
	result, err := s.repo.RevertTurn(ctx, owner, sessionID, turnID, expectedHeadID, time.Now().UTC())
	if err == nil {
		s.controls.changed(owner, sessionID)
	}
	return result, err
}

func (s *Service) Settings(ctx context.Context, owner string) (Settings, error) {
	return s.settingsFor(ctx, owner, s.resolve(""))
}

func (s *Service) ComposerDefaults(ctx context.Context, owner string) (*ComposerDefaults, error) {
	return s.repo.ComposerDefaults(ctx, owner)
}

func (s *Service) SetComposerDefaults(ctx context.Context, owner string, defaults ComposerDefaults) error {
	options := Options{Model: defaults.Model, Permission: defaults.Permission, Thinking: defaults.Thinking}
	if defaults.Permission == "" || defaults.Thinking == "" || !validOptions(options) {
		return ErrOptions
	}
	return s.repo.SaveComposerDefaults(ctx, owner, defaults)
}

func (s *Service) settingsFor(ctx context.Context, owner string, target Target) (Settings, error) {
	available := target.policies(owner)
	if len(available) == 0 {
		return Settings{Available: available}, nil
	}
	saved, err := s.repo.Policy(ctx, owner)
	if err != nil {
		return Settings{}, err
	}
	if !slices.Contains(available, saved) {
		saved = available[0]
	}
	return Settings{Available: available, Selected: saved}, nil
}

func (s *Service) SetPolicy(ctx context.Context, owner string, choice sandbox.Policy) (Settings, error) {
	available := s.resolve("").policies(owner)
	if !slices.Contains(available, choice) {
		return Settings{}, ErrPolicy
	}
	if err := s.repo.SetPolicy(ctx, owner, choice); err != nil {
		return Settings{}, err
	}
	s.controls.changedAccount(owner)
	return Settings{Available: available, Selected: choice}, nil
}
