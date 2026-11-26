package auth

import (
	"context"
	"crypto/subtle"
	"errors"
	"time"

	"github.com/OXeu/shinona/internal/timing"
)

var (
	ErrOAuthDisabled = errors.New("OAuth sign-in is not configured")
	ErrOAuthLinked   = errors.New("the account already has an OAuth identity")
	ErrOAuthInUse    = errors.New("the OAuth identity belongs to another account")
	ErrOAuthPassword = errors.New("set a password before disconnecting OAuth")
)

func (s *Service) HasOAuthIdentity(ctx context.Context) (bool, error) {
	defer timing.Start(ctx, "account_oauth_status")()
	return s.repo.HasOAuthIdentity(ctx, CurrentUser(ctx).ID)
}

func (s *Service) BeginOAuthLink(ctx context.Context, version, token, password string) (string, Flow, error) {
	defer timing.Start(ctx, "account_oauth_begin")()
	state, err := s.verifyCredentials(ctx, token, password, time.Now().UTC())
	if err != nil {
		return "", Flow{}, err
	}
	linked, err := s.repo.HasOAuthIdentity(ctx, state.User.ID)
	if err != nil {
		return "", Flow{}, err
	}
	if linked {
		return "", Flow{}, ErrOAuthLinked
	}
	return s.beginFlow(ctx, version, state.User.ID, digest(token))
}

func (f Flow) MatchesSession(token string) bool {
	return token != "" && f.OwnerID != "" && subtle.ConstantTimeCompare([]byte(f.SessionKey), []byte(digest(token))) == 1
}

func (s *Service) FinishOAuthLink(ctx context.Context, flow Flow, token string, identity Identity) error {
	defer timing.Start(ctx, "account_oauth_link")()
	if !flow.MatchesSession(token) || identity.Issuer == "" || identity.Subject == "" {
		return ErrCredentials
	}
	return s.repo.LinkOAuthIdentity(ctx, flow.OwnerID, flow.SessionKey, identity, time.Now().UTC())
}

func (s *Service) UnlinkOAuth(ctx context.Context, token, password string) error {
	defer timing.Start(ctx, "account_oauth_unlink")()
	if !CurrentUser(ctx).HasPassword || CurrentUser(ctx).LoginEmail == "" {
		return ErrOAuthPassword
	}
	now := time.Now().UTC()
	state, err := s.verifyCredentials(ctx, token, password, now)
	if err != nil {
		return err
	}
	return s.repo.UnlinkOAuthIdentity(ctx, state, digest(token), now)
}
