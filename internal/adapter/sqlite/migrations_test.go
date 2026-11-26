package sqlite

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/sqlite"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
)

func openSchemaFixture(t *testing.T, filename string, version int) *Database {
	t.Helper()
	dsn, _, err := databaseDSNs(filename)
	if err != nil {
		t.Fatal(err)
	}
	db, pool, err := openDB(sqlite.DriverName, dsn, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := migrateTo(db.WithContext(t.Context()), version); err != nil {
		t.Fatal(err)
	}
	return newDatabase(db, db)
}

func markUnversioned(t *testing.T, repo *Database) {
	t.Helper()
	if err := repo.db.Exec("PRAGMA user_version = 0").Error; err != nil {
		t.Fatal(err)
	}
}

func TestMigratesAuthenticationTimeFromLegacyExpiry(t *testing.T) {
	ctx := t.Context()
	filename := filepath.Join(t.TempDir(), "legacy.db")
	old := openSchemaFixture(t, filename, 3)
	const owner, key = "owner", "legacy-session"
	user := auth.User{ID: owner, Email: "owner@example.com", Name: "Owner"}
	if err := old.Accounts.CreatePasswordUser(ctx, user, []byte("stored-hash")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	signedIn := now.Add(-2 * time.Hour)
	if err := old.Accounts.SaveSession(ctx, key, owner, signedIn.Add(auth.SessionLifetime)); err != nil {
		t.Fatal(err)
	}
	if err := old.db.Exec("ALTER TABLE login_sessions DROP COLUMN authenticated_at").Error; err != nil {
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
	state, err := repo.Accounts.AccountCredentials(ctx, owner, key, now)
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
	old := openSchemaFixture(t, filename, 2)
	seedAccounts(t, old, "owner")
	now := time.Now().UTC()
	if err := old.Conversations.CreateSession(ctx, run.Session{OwnerID: "owner", ID: "session", CreatedAt: now, UpdatedAt: now}); err != nil {
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
	image, err := repo.Conversations.Attachment(ctx, "owner", "session", "turn", 0)
	if err != nil || image == nil || image.Data != encoded || image.Name != "shot.png" {
		t.Fatalf("migrated attachment: %+v, %v", image, err)
	}
	turn := run.Run{OwnerID: "owner", SessionID: "session", Sequence: 2}
	history, err := repo.Conversations.History(ctx, turn, 0)
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

func TestOwnershipMigrationKeepsValidDataAndRemovesLegacyGuards(t *testing.T) {
	ctx := t.Context()
	repo := openSchemaFixture(t, filepath.Join(t.TempDir(), "v5.db"), 5)
	seedAccounts(t, repo, "owner", "other")
	now := time.Now().UTC()
	if err := repo.Projects.CreateProject(ctx, run.Project{ID: "project", OwnerID: "owner", Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Conversations.CreateSession(ctx, run.Session{ID: "session", OwnerID: "owner", ProjectID: "project", Title: "Saved"}); err != nil {
		t.Fatal(err)
	}
	for _, binding := range []string{"project", "", "deleted-project"} {
		if saved, err := repo.E2B.SaveE2BSandbox(ctx, "owner", binding, "endpoint", "", "box", []byte("credentials")); err != nil || !saved {
			t.Fatalf("fixture binding %q: %t, %v", binding, saved, err)
		}
	}
	for _, item := range []any{
		&row{ID: "legacy-run", SessionID: "unused-legacy-id", Prompt: "Import me", Status: run.Succeeded, CreatedAt: now, UpdatedAt: now},
		&flowRow{Key: "anonymous", Nonce: "nonce", Verifier: "verifier", ExpiresAt: now.Add(time.Hour)},
		&flowRow{Key: "linked", OwnerID: "owner", SessionKey: "login", Nonce: "nonce", Verifier: "verifier", ExpiresAt: now.Add(time.Hour)},
	} {
		if err := repo.db.Create(item).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := repo.db.Exec(`INSERT INTO deleted_accounts (id) VALUES ('gone');
		INSERT INTO session_list_revisions (owner_id, revision) VALUES ('gone', 4);
		CREATE TRIGGER reject_deleted_owner_model_settings BEFORE INSERT ON model_settings
			WHEN EXISTS (SELECT 1 FROM deleted_accounts WHERE id = NEW.owner_id)
			BEGIN SELECT RAISE(ABORT, 'account was deleted'); END;
		CREATE INDEX extra_turn_index ON runs (owner_id, created_at);
		CREATE VIEW project_names AS SELECT id, name FROM projects;
		CREATE TABLE imported_projects (id TEXT);
		CREATE TRIGGER import_project AFTER INSERT ON imported_projects
			BEGIN INSERT INTO projects (id, owner_id, name) VALUES (NEW.id, 'owner', 'Imported'); END;`).Error; err != nil {
		t.Fatal(err)
	}
	expectedRevision, err := repo.Conversations.SessionListRevision(ctx, "owner")
	if err != nil || expectedRevision == 0 {
		t.Fatalf("fixture revision = %d, %v", expectedRevision, err)
	}
	if err := migrateTo(repo.db.WithContext(ctx), 6); err != nil {
		t.Fatal(err)
	}
	if version, err := schemaVersion(repo.db); err != nil || version != 6 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if revision, err := repo.Conversations.SessionListRevision(ctx, "owner"); err != nil || revision != expectedRevision {
		t.Fatalf("migration changed preserved revision: %d, %v", revision, err)
	}
	if revision, err := repo.Conversations.SessionListRevision(ctx, "gone"); err != nil || revision != 0 {
		t.Fatalf("orphaned revision survived: %d, %v", revision, err)
	}
	if repo.db.Migrator().HasTable("deleted_accounts") || !repo.db.Migrator().HasIndex(&row{}, "extra_turn_index") {
		t.Fatal("migration retained obsolete guards or lost a named index")
	}
	if err := repo.db.Exec("INSERT INTO imported_projects VALUES ('imported')").Error; err != nil {
		t.Fatal(err)
	}
	var projectName string
	if err := repo.db.Raw("SELECT name FROM project_names WHERE id = 'imported'").Row().Scan(&projectName); err != nil ||
		projectName != "Imported" {
		t.Fatalf("migration did not preserve custom schema references: %q, %v", projectName, err)
	}
	for key, owner := range map[string]string{"anonymous": "", "linked": "owner"} {
		flow, err := repo.Accounts.ConsumeFlow(ctx, key, now)
		if err != nil || flow == nil || flow.OwnerID != owner || flow.Verifier != "verifier" {
			t.Fatalf("migrated flow %s: %+v, %v", key, flow, err)
		}
	}
	if count, err := repo.Conversations.AdoptLegacy(ctx, "owner"); err != nil || count != 1 {
		t.Fatalf("legacy adoption = %d, %v", count, err)
	}
	var boxes []e2bWorkspaceRow
	if err := repo.db.Order("project_id").Find(&boxes).Error; err != nil || len(boxes) != 2 ||
		boxes[0].ProjectID != "" || boxes[1].ProjectID != "project" || string(boxes[1].Credentials) != "credentials" {
		t.Fatalf("migration did not preserve existing E2B bindings: %d rows, %v", len(boxes), err)
	}
	if _, err := repo.E2B.SaveE2BSandbox(ctx, "other", "project", "endpoint", "", "box", []byte("credentials")); err == nil {
		t.Fatal("migration did not restore foreign key enforcement on the writer")
	}
}

func TestOwnershipMigrationRollsBackInvalidReferences(t *testing.T) {
	ctx := t.Context()
	repo := openSchemaFixture(t, filepath.Join(t.TempDir(), "v5.db"), 5)
	seedAccounts(t, repo, "owner", "other")
	if err := repo.Projects.CreateProject(ctx, run.Project{ID: "project", OwnerID: "owner", Name: "Project"}); err != nil {
		t.Fatal(err)
	}
	if err := repo.db.Create(&sessionRow{ID: "session", OwnerID: "other", ProjectID: "project", Title: "Recoverable"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrate(repo.db.WithContext(ctx)); err == nil ||
		!strings.Contains(err.Error(), "agent_sessions references a missing row in projects") {
		t.Fatalf("invalid ownership upgrade: %v", err)
	}
	if version, err := schemaVersion(repo.db); err != nil || version != 5 {
		t.Fatalf("migration committed a partial schema: %d, %v", version, err)
	}
	var saved sessionRow
	if err := repo.db.Take(&saved, "id = ?", "session").Error; err != nil ||
		saved.ProjectID != "project" || saved.OwnerID != "other" || saved.Title != "Recoverable" {
		t.Fatalf("migration modified the source data: %+v, %v", saved, err)
	}
	var enforced int
	if err := repo.db.Raw("PRAGMA foreign_keys").Row().Scan(&enforced); err != nil || enforced != 1 {
		t.Fatalf("writer foreign keys after rollback = %d, %v", enforced, err)
	}
	if err := repo.db.Model(&saved).UpdateColumn("project_id", "").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrate(repo.db.WithContext(ctx)); err != nil {
		t.Fatalf("migration could not retry after fixing ownership: %v", err)
	}
}

func TestOwnershipMigrationRunsOnceOnConcurrentOpen(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "v5.db")
	old := openSchemaFixture(t, filename, 5)
	seedAccounts(t, old, "owner")
	newSession(t, old, "owner", "session")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	results := make(chan error, 2)
	for range 2 {
		go func() {
			repo, err := Open(ctx, filename)
			if err == nil {
				err = repo.Close()
			}
			results <- err
		}()
	}
	for range 2 {
		if err := <-results; err != nil {
			t.Error(err)
		}
	}
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	sessions, err := repo.Conversations.RecentSessions(ctx, "owner", nil, 10)
	if err != nil || len(sessions) != 1 || sessions[0].ID != "session" {
		t.Fatalf("concurrent upgrades changed the session data: %+v, %v", sessions, err)
	}
}
