package sqlite

import (
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/auth"
)

func TestEmailChallengeAttemptLimitSurvivesConcurrentWrites(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "email.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	const owner, key = "owner", "session"
	now := time.Now().UTC()
	seedAccounts(t, repo, owner)
	if err := repo.Accounts.SaveSession(ctx, key, owner, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	account, err := repo.Accounts.AccountCredentials(ctx, owner, key, now)
	if err != nil {
		t.Fatal(err)
	}
	challenge := auth.EmailChallenge{
		ID: "first", OwnerID: owner, SessionKey: key, Email: "replacement@example.test",
		CodeHash: "correct", SentAt: now, ExpiresAt: now.Add(auth.EmailLifetime),
	}
	if err := repo.Accounts.SaveEmailChallenge(ctx, account, challenge); err != nil {
		t.Fatal(err)
	}

	verify := func(id, hash string, at time.Time) error {
		return repo.Accounts.VerifyEmailChallenge(ctx, owner, key, id, hash, at)
	}
	if err := repo.db.Exec(`CREATE TRIGGER reject_attempt BEFORE UPDATE OF attempts ON account_email_challenges
		BEGIN SELECT RAISE(ABORT, 'cannot save attempt'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := verify(challenge.ID, "incorrect", now); err == nil || errors.Is(err, auth.ErrEmailCode) {
		t.Fatalf("verification hid a failed attempt write: %v", err)
	}
	if err := repo.db.Exec("DROP TRIGGER reject_attempt").Error; err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			if err := verify(challenge.ID, "incorrect", now); !errors.Is(err, auth.ErrEmailCode) {
				t.Errorf("incorrect challenge code: %v", err)
			}
		})
	}
	workers.Wait()
	var stored emailChallengeRow
	if err := repo.db.Take(&stored, "owner_id = ?", owner).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Attempts != 5 {
		t.Fatalf("saved %d incorrect attempts, expected five", stored.Attempts)
	}
	if err := verify(challenge.ID, challenge.CodeHash, now); !errors.Is(err, auth.ErrEmailCode) {
		t.Fatalf("exhausted challenge accepted the correct code: %v", err)
	}

	replacement := challenge
	replacement.ID = "second"
	replacement.SentAt = now.Add(time.Minute - time.Millisecond)
	replacement.ExpiresAt = replacement.SentAt.Add(auth.EmailLifetime)
	if err := repo.Accounts.SaveEmailChallenge(ctx, account, replacement); !errors.Is(err, auth.ErrEmailWait) {
		t.Fatalf("replacement before cooldown: %v", err)
	}
	replacement.SentAt = now.Add(time.Minute)
	replacement.ExpiresAt = replacement.SentAt.Add(auth.EmailLifetime)
	if err := repo.Accounts.SaveEmailChallenge(ctx, account, replacement); err != nil {
		t.Fatalf("replacement after cooldown: %v", err)
	}
	if err := verify(replacement.ID, replacement.CodeHash, replacement.ExpiresAt); !errors.Is(err, auth.ErrEmailCode) {
		t.Fatalf("expired challenge accepted the correct code: %v", err)
	}
	if err := verify(replacement.ID, replacement.CodeHash, replacement.ExpiresAt.Add(-time.Millisecond)); err != nil {
		t.Fatalf("replacement challenge lost its attempt budget: %v", err)
	}
}
