package sqlite

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

func asLegacyDatabase(t *testing.T, repo *Repository) {
	t.Helper()
	if err := repo.db.Exec("DROP TABLE run_attachments; PRAGMA user_version = 0").Error; err != nil {
		t.Fatal(err)
	}
}

func TestMigratesAuthenticationTimeFromLegacyExpiry(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "legacy.db")
	old, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = old.Close() })
	const owner, key = "owner", "legacy-session"
	user := auth.User{ID: owner, Email: "owner@example.com", Name: "Owner"}
	if err := old.CreatePasswordUser(ctx, user, []byte("stored-hash")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	signedIn := now.Add(-2 * time.Hour)
	if err := old.SaveSession(ctx, key, owner, signedIn.Add(auth.SessionLifetime)); err != nil {
		t.Fatal(err)
	}
	if err := old.db.Exec("ALTER TABLE login_sessions DROP COLUMN authenticated_at; PRAGMA user_version = 3").Error; err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	state, err := repo.AccountCredentials(ctx, owner, key, now)
	if err != nil {
		t.Fatal(err)
	}
	if !state.AuthenticatedAt.Equal(signedIn) {
		t.Fatalf("migrated authentication time = %v, want %v", state.AuthenticatedAt, signedIn)
	}
}

func TestMigratesInlineAttachmentsWithoutChangingHistory(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "legacy.db")
	old, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := old.CreateSession(ctx, run.Session{OwnerID: "owner", ID: "session", CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := old.db.Create(&row{ID: "turn", OwnerID: "owner", SessionID: "session", Sequence: 1, Status: run.Succeeded,
		Prompt: "image", CreatedAt: now, UpdatedAt: now,
	}).Error; err != nil {
		t.Fatal(err)
	}
	const encoded = "iVBORw0KGgo="
	const payload = `{"images":[{"name":"shot.png","mediaType":"image/png","data":"` + encoded + `","extension":"keep"}],"extra":true,"options":{"Model":"fixture"}}`
	if err := old.db.Model(&row{}).Where("id = ?", "turn").UpdateColumn("payload", payload).Error; err != nil {
		t.Fatal(err)
	}
	asLegacyDatabase(t, old)
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	var retained string
	if err := repo.db.Raw(`SELECT json_extract(payload, '$.images[0].extension') FROM runs WHERE id = 'turn' AND json_extract(payload, '$.extra') = 1 AND json_type(payload, '$.images[0].data') IS NULL`).
		Row().Scan(&retained); err != nil || retained != "keep" {
		t.Fatalf("migration lost extension fields or kept inline bytes: %q, %v", retained, err)
	}
	image, err := repo.Attachment(ctx, "owner", "session", "turn", 0)
	if err != nil || image == nil || image.Data != encoded || image.Name != "shot.png" {
		t.Fatalf("migrated attachment: %+v, %v", image, err)
	}
	turn := run.Run{OwnerID: "owner", SessionID: "session", Sequence: 2}
	history, err := repo.History(ctx, turn, 0)
	if err != nil || len(history) != 1 || len(history[0].Images) != 1 || history[0].Images[0].Data != encoded {
		t.Fatalf("migrated history: %+v, %v", history, err)
	}
	if err := repo.db.Delete(&row{}, "id = ?", "turn").Error; err != nil {
		t.Fatal(err)
	}
	var remaining int64
	if err := repo.db.Model(&runAttachmentRow{}).Count(&remaining).Error; err != nil || remaining != 0 {
		t.Fatalf("attachment escaped its foreign key cascade: %d, %v", remaining, err)
	}
}
