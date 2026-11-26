// Package auth owns accounts, sign-in sessions, and OAuth login attempts.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"
	"time"

	"github.com/OXeu/shinona/internal/timing"
)

const SessionLifetime = 7 * 24 * time.Hour
const FlowLifetime = 10 * time.Minute
const MinPasswordBytes = 12
const MaxPasswordBytes = 128

var (
	ErrCredentials = errors.New("email or password is incorrect")
	ErrPassword    = errors.New("use a password between 12 and 128 bytes")
	ErrEmail       = errors.New("enter a valid email address")
)

type User struct {
	ID            string
	Email         string
	LoginEmail    string
	Name          string
	AvatarURL     string
	AvatarVersion string
	HasPassword   bool
	Admin         bool
	Disabled      bool
}

type Identity struct {
	Issuer    string
	Subject   string
	Email     string
	Name      string
	AvatarURL string
}

type Flow struct {
	Key        string
	Nonce      string
	Verifier   string
	ExpiresAt  time.Time
	Version    string
	OwnerID    string
	SessionKey string
}

type Repository interface {
	AccountRepository
	CreatePasswordUser(context.Context, User, []byte) error
	PasswordUser(context.Context, string) (User, []byte, error)
	OAuthUser(context.Context, Identity, string) (User, error)
	SaveSession(context.Context, string, string, time.Time) error
	SaveOAuthSession(context.Context, string, string, Identity, time.Time) error
	SessionUser(context.Context, string, time.Time) (*User, error)
	DeleteSession(context.Context, string) error
	SaveFlow(context.Context, Flow, time.Time) error
	ConsumeFlow(context.Context, string, time.Time) (*Flow, error)
}

type PasswordHasher interface {
	Hash(string) []byte
	Match(string, []byte) bool
}

type Service struct {
	repo  Repository
	hash  PasswordHasher
	decoy []byte
	newID func() string
}

func New(repo Repository, hash PasswordHasher, newID func() string) *Service {
	return &Service{repo: repo, hash: hash, decoy: hash.Hash(rand.Text()), newID: newID}
}

func (s *Service) CreatePasswordUser(ctx context.Context, email, password string) (User, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return User{}, ErrPassword
	}
	user := User{ID: s.newID(), Email: email, LoginEmail: email, HasPassword: true}
	return user, s.repo.CreatePasswordUser(ctx, user, s.hash.Hash(password))
}

func (s *Service) PasswordUser(ctx context.Context, email, password string) (User, error) {
	email, err := normalizeEmail(email)
	if err != nil || len(password) > MaxPasswordBytes {
		return User{}, ErrCredentials
	}
	lookup := timing.Start(ctx, "auth_password_lookup")
	user, hash, err := s.repo.PasswordUser(ctx, email)
	lookup()
	if err != nil {
		return User{}, err
	}
	if user.ID == "" || len(hash) == 0 {
		hash = s.decoy
	}
	verify := timing.Start(ctx, "auth_password_verify")
	matches := s.hash.Match(password, hash)
	verify()
	if !matches || user.ID == "" || !user.HasPassword || user.Disabled {
		return User{}, ErrCredentials
	}
	return user, nil
}

func (s *Service) OAuthUser(ctx context.Context, identity Identity) (User, error) {
	if identity.Issuer == "" || identity.Subject == "" {
		return User{}, ErrCredentials
	}
	identity.Name, _ = normalizeName(identity.Name)
	defer timing.Start(ctx, "auth_oauth_account")()
	user, err := s.repo.OAuthUser(ctx, identity, s.newID())
	if err == nil && user.Disabled {
		return User{}, ErrCredentials
	}
	return user, err
}

func (s *Service) StartSession(ctx context.Context, user User) (string, error) {
	defer timing.Start(ctx, "auth_session_create")()
	token := rand.Text() + rand.Text()
	if err := s.repo.SaveSession(ctx, digest(token), user.ID, time.Now().UTC().Add(SessionLifetime)); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Service) StartOAuthSession(ctx context.Context, user User, identity Identity) (string, error) {
	defer timing.Start(ctx, "auth_session_create")()
	token := rand.Text() + rand.Text()
	if err := s.repo.SaveOAuthSession(ctx, digest(token), user.ID, identity, time.Now().UTC().Add(SessionLifetime)); err != nil {
		return "", err
	}
	return token, nil
}

func (s *Service) SessionUser(ctx context.Context, token string) (*User, error) {
	if token == "" {
		return nil, nil
	}
	return s.repo.SessionUser(ctx, digest(token), time.Now().UTC())
}

func (s *Service) EndSession(ctx context.Context, token string) error {
	if token == "" {
		return nil
	}
	defer timing.Start(ctx, "auth_session_delete")()
	return s.repo.DeleteSession(ctx, digest(token))
}

func (s *Service) BeginFlow(ctx context.Context, version string) (string, Flow, error) {
	defer timing.Start(ctx, "auth_oauth_begin")()
	return s.beginFlow(ctx, version, "", "")
}

func (s *Service) beginFlow(ctx context.Context, version, owner, sessionKey string) (string, Flow, error) {
	state := rand.Text() + rand.Text()
	now := time.Now().UTC()
	flow := Flow{
		Key: digest(state), Nonce: rand.Text(), Verifier: rand.Text() + rand.Text(),
		ExpiresAt: now.Add(FlowLifetime), Version: version, OwnerID: owner, SessionKey: sessionKey,
	}
	return state, flow, s.repo.SaveFlow(ctx, flow, now)
}

func (s *Service) ConsumeFlow(ctx context.Context, state string) (*Flow, error) {
	defer timing.Start(ctx, "auth_oauth_consume")()
	return s.repo.ConsumeFlow(ctx, digest(state), time.Now().UTC())
}

func normalizeEmail(value string) (string, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	address, err := mail.ParseAddress(value)
	if err != nil || address.Address != value || len(value) > 254 {
		return "", ErrEmail
	}
	return value, nil
}

func digest(value string) string {
	hash := sha256.Sum256([]byte(value))
	return hex.EncodeToString(hash[:])
}

type userKey struct{}

func WithUser(ctx context.Context, user User) context.Context {
	return context.WithValue(ctx, userKey{}, user)
}

func CurrentUser(ctx context.Context) User {
	user, _ := ctx.Value(userKey{}).(User)
	return user
}
