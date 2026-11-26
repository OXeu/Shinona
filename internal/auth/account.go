package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/timing"
)

const (
	MaxAvatarBytes  = 512 * 1024
	EmailLifetime   = 10 * time.Minute
	emailCooldown   = time.Minute
	maxCodeAttempts = 5
	recentSignIn    = 15 * time.Minute
)

var (
	ErrName           = errors.New("enter a nickname of up to 60 characters")
	ErrAvatar         = errors.New("select a PNG, JPEG or WebP image of up to 512 KiB")
	ErrCurrent        = errors.New("the current password is incorrect")
	ErrRecentSignIn   = errors.New("sign in again before changing account credentials")
	ErrBindEmail      = errors.New("verify a sign-in email before setting a password")
	ErrEmailInUse     = errors.New("this sign-in email is already in use")
	ErrEmailUnchanged = errors.New("this sign-in email is already bound to your account")
	ErrEmailCode      = errors.New("the verification code is invalid or expired")
	ErrEmailWait      = errors.New("wait one minute before requesting another code")
	ErrEmailDisabled  = errors.New("email delivery is not configured")
	ErrEmailDelivery  = errors.New("could not send the verification email")
	ErrAccountChanged = errors.New("the account changed while processing the request")
	ErrAdminRequired  = errors.New("super administrator access is required")
	ErrAdminSelf      = errors.New("you cannot change your own administrator role or account access")
	ErrAccountMissing = errors.New("account not found")
)

type Avatar struct {
	Version   string
	MediaType string
	Data      []byte
}

type ProfileChange struct {
	Name   string
	Avatar *Avatar // nil keeps the current avatar. An empty avatar restores the provider image.
}

type CredentialState struct {
	User            User
	PasswordHash    []byte
	AuthenticatedAt time.Time
}

type EmailChallenge struct {
	ID         string
	OwnerID    string
	SessionKey string
	Email      string
	CodeHash   string
	SentAt     time.Time
	ExpiresAt  time.Time
	Attempts   int
}

func (c EmailChallenge) CheckResend(now time.Time) error {
	if now.Before(c.ExpiresAt) && c.SentAt.After(now.Add(-emailCooldown)) {
		return ErrEmailWait
	}
	return nil
}

// VerifyHash consumes one attempt when a live challenge receives the wrong code.
func (c *EmailChallenge) VerifyHash(hash string, now time.Time) error {
	if !now.Before(c.ExpiresAt) || c.Attempts >= maxCodeAttempts {
		return ErrEmailCode
	}
	if subtle.ConstantTimeCompare([]byte(c.CodeHash), []byte(hash)) != 1 {
		c.Attempts++
		return ErrEmailCode
	}
	return nil
}

type AccountPage struct {
	Users []User
	Total int64
}

type AccountAccess struct {
	Admin    *bool
	Disabled *bool
}

type AccountRepository interface {
	UpdateProfile(ctx context.Context, owner string, change ProfileChange) (User, error)
	AccountAvatar(ctx context.Context, owner, version string) (*Avatar, error)
	AccountCredentials(ctx context.Context, owner, sessionKey string, now time.Time) (CredentialState, error)
	ChangePassword(ctx context.Context, state CredentialState, sessionKey string, hash []byte, now time.Time) error
	SaveEmailChallenge(ctx context.Context, state CredentialState, challenge EmailChallenge) error
	VerifyEmailChallenge(ctx context.Context, owner, sessionKey, id, codeHash string, now time.Time) error
	ListAccounts(ctx context.Context, search string, offset, limit int) (AccountPage, error)
	CreateManagedAccount(ctx context.Context, actor string, user User, hash []byte) error
	SetAccountAccess(ctx context.Context, actor, target string, access AccountAccess) (User, error)
	DeleteAccount(ctx context.Context, actor, target string) error
	HasOAuthIdentity(ctx context.Context, owner string) (bool, error)
	LinkOAuthIdentity(ctx context.Context, owner, sessionKey string, identity Identity, now time.Time) error
	UnlinkOAuthIdentity(ctx context.Context, state CredentialState, sessionKey string, now time.Time) error
}

type VerificationMailer interface {
	SendVerification(ctx context.Context, verification EmailVerification) error
}

func (s *Service) UpdateProfile(ctx context.Context, change ProfileChange) (User, error) {
	defer timing.Start(ctx, "account_profile_update")()
	name, err := normalizeName(change.Name)
	if err != nil {
		return User{}, err
	}
	change.Name = name
	if change.Avatar != nil && len(change.Avatar.Data) > 0 {
		change.Avatar.Version = s.newID()
	}
	return s.repo.UpdateProfile(ctx, CurrentUser(ctx).ID, change)
}

func (s *Service) Avatar(ctx context.Context, owner, version string) (*Avatar, error) {
	defer timing.Start(ctx, "account_avatar_load")()
	actor := CurrentUser(ctx)
	if actor.ID != owner && !actor.Admin {
		return nil, ErrAccountMissing
	}
	return s.repo.AccountAvatar(ctx, owner, version)
}

func (s *Service) ChangePassword(ctx context.Context, token, current, password string) error {
	defer timing.Start(ctx, "account_password_change")()
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return ErrPassword
	}
	now := time.Now().UTC()
	state, err := s.verifyCredentials(ctx, token, current, now)
	if err != nil {
		return err
	}
	if state.User.LoginEmail == "" {
		return ErrBindEmail
	}
	hashPassword := timing.Start(ctx, "account_password_hash")
	hash := s.hash.Hash(password)
	hashPassword()
	return s.repo.ChangePassword(ctx, state, digest(token), hash, now)
}

type EmailVerification struct {
	RequestID, Address, Code string
	ExpiresAt                time.Time
}

func (s *Service) PrepareEmailCode(ctx context.Context, token, current, email string) (EmailVerification, error) {
	defer timing.Start(ctx, "account_email_begin")()
	email, err := normalizeEmail(email)
	if err != nil {
		return EmailVerification{}, err
	}
	now := time.Now().UTC()
	state, err := s.verifyCredentials(ctx, token, current, now)
	if err != nil {
		return EmailVerification{}, err
	}
	if state.User.LoginEmail == email {
		return EmailVerification{}, ErrEmailUnchanged
	}
	number, err := rand.Int(rand.Reader, big.NewInt(100_000_000))
	if err != nil {
		return EmailVerification{}, err
	}
	code := fmt.Sprintf("%08d", number.Int64())
	id := rand.Text()
	challenge := EmailChallenge{ID: id, OwnerID: state.User.ID, SessionKey: digest(token), Email: email,
		CodeHash: emailCodeHash(token, id, code), SentAt: now, ExpiresAt: now.Add(EmailLifetime)}
	if err := s.repo.SaveEmailChallenge(ctx, state, challenge); err != nil {
		return EmailVerification{}, err
	}
	return EmailVerification{RequestID: id, Address: email, Code: code, ExpiresAt: challenge.ExpiresAt}, nil
}

func (v EmailVerification) Send(ctx context.Context, mailer VerificationMailer) error {
	send := timing.Start(ctx, "account_email_delivery")
	err := mailer.SendVerification(ctx, v)
	send()
	if err != nil {
		return errors.Join(ErrEmailDelivery, err)
	}
	return nil
}

func (s *Service) VerifyEmailCode(ctx context.Context, token, id, code string) error {
	defer timing.Start(ctx, "account_email_verify")()
	if id == "" || len(id) > 128 || len(code) != 8 || strings.Trim(code, "0123456789") != "" {
		return ErrEmailCode
	}
	return s.repo.VerifyEmailChallenge(ctx, CurrentUser(ctx).ID, digest(token), id, emailCodeHash(token, id, code), time.Now().UTC())
}

func (s *Service) verifyCredentials(ctx context.Context, token, current string, now time.Time) (CredentialState, error) {
	load := timing.Start(ctx, "account_credentials_load")
	state, err := s.repo.AccountCredentials(ctx, CurrentUser(ctx).ID, digest(token), now)
	load()
	if err != nil {
		return state, err
	}
	if len(state.PasswordHash) == 0 {
		if state.AuthenticatedAt.Before(now.Add(-recentSignIn)) {
			return state, ErrRecentSignIn
		}
		return state, nil
	}
	verify := timing.Start(ctx, "account_current_password")
	valid := len(current) <= MaxPasswordBytes && s.hash.Match(current, state.PasswordHash)
	verify()
	if !valid {
		return state, ErrCurrent
	}
	return state, nil
}

func (s *Service) ListAccounts(ctx context.Context, search string, offset, limit int) (AccountPage, error) {
	defer timing.Start(ctx, "account_admin_list")()
	if !CurrentUser(ctx).Admin {
		return AccountPage{}, ErrAdminRequired
	}
	return s.repo.ListAccounts(ctx, strings.TrimSpace(search), offset, limit)
}

func (s *Service) CreateManagedAccount(ctx context.Context, email, password, name string) (User, error) {
	defer timing.Start(ctx, "account_admin_create")()
	actor := CurrentUser(ctx)
	if !actor.Admin {
		return User{}, ErrAdminRequired
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return User{}, err
	}
	name, err = normalizeName(name)
	if err != nil {
		return User{}, err
	}
	if len(password) < MinPasswordBytes || len(password) > MaxPasswordBytes {
		return User{}, ErrPassword
	}
	user := User{ID: s.newID(), Email: email, LoginEmail: email, Name: name, HasPassword: true}
	return user, s.repo.CreateManagedAccount(ctx, actor.ID, user, s.hash.Hash(password))
}

func (s *Service) SetAccountAccess(ctx context.Context, target string, access AccountAccess) (User, error) {
	defer timing.Start(ctx, "account_admin_access")()
	actor, err := managementActor(ctx, target)
	if err != nil {
		return User{}, err
	}
	return s.repo.SetAccountAccess(ctx, actor, target, access)
}

func (s *Service) DeleteAccount(ctx context.Context, target string) error {
	defer timing.Start(ctx, "account_admin_delete")()
	actor, err := managementActor(ctx, target)
	if err != nil {
		return err
	}
	return s.repo.DeleteAccount(ctx, actor, target)
}

func managementActor(ctx context.Context, target string) (string, error) {
	actor := CurrentUser(ctx)
	if !actor.Admin {
		return "", ErrAdminRequired
	}
	if target == actor.ID {
		return "", ErrAdminSelf
	}
	return actor.ID, nil
}

func normalizeName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if !utf8.ValidString(name) || utf8.RuneCountInString(name) > 60 || strings.ContainsFunc(name, unicode.IsControl) {
		return "", ErrName
	}
	return name, nil
}

func emailCodeHash(token, id, code string) string {
	hash := hmac.New(sha256.New, []byte(token))
	_, _ = hash.Write([]byte(id + ":" + code))
	return hex.EncodeToString(hash.Sum(nil))
}
