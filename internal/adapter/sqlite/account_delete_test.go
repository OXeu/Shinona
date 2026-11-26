package sqlite

import (
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestDeleteAccountClearsOwnedRowsAndRejectsLateWrites(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := repo.Close(); err != nil {
			t.Error(err)
		}
	})
	now := time.Now().UTC()
	data := []byte("{}")
	db := repo.db.WithContext(ctx)
	const owner, admin = "target-account", "admin-account"
	for _, item := range []any{
		&userRow{ID: owner, Email: "user@example.test"},
		&userRow{ID: admin, Email: "admin@example.test", Admin: true},
		&projectRow{ID: "project", OwnerID: owner, Name: "Project"},
		&projectRow{ID: "other-project", OwnerID: admin, Name: "Project"},
		&sessionRow{ID: "chat", OwnerID: owner, ProjectID: "project", CreatedAt: now, UpdatedAt: now},
		&workspaceRootRow{OwnerID: owner, ProjectID: "project", Root: "src"},
		&legacyWorkspaceRootRow{OwnerID: owner, SessionID: "chat", Root: "old-src"},
		&row{ID: "running-turn", OwnerID: owner, SessionID: "chat", Status: run.Running, CreatedAt: now, UpdatedAt: now},
		&settingsRow{OwnerID: owner},
		&composerDefaultsRow{OwnerID: owner},
		&modelSettingsRow{OwnerID: owner, Data: data},
		&mcpSettingsRow{OwnerID: owner, Data: data},
		&mcpGrantRow{OwnerID: owner, Server: "server", Grant: data},
		&e2bWorkspaceRow{OwnerID: owner, ProjectID: "project", Endpoint: "fixture", SandboxID: "box", Credentials: data},
		&projectSandboxRow{OwnerID: owner, ProjectID: "project", ProjectSandboxConfig: run.ProjectSandboxConfig{Provider: "e2b"}},
		&sshServerRow{OwnerID: owner, ID: "fixture", Name: "Server", PrivateKey: "private"},
		&avatarRow{OwnerID: owner},
		&emailChallengeRow{OwnerID: owner},
		&identityRow{OwnerID: owner, Issuer: "example", Subject: "subject"},
		&loginRow{OwnerID: owner, Key: "session-key", ExpiresAt: now.Add(time.Hour)},
		&flowRow{OwnerID: owner, Key: "flow", ExpiresAt: now.Add(time.Hour)},
		&instanceSettingsRow{ID: instanceSettingsID, Data: data},
	} {
		if err := db.Create(item).Error; err != nil {
			t.Fatalf("seed %T: %v", item, err)
		}
	}

	if err := db.Exec(`CREATE TRIGGER fail_delete BEFORE DELETE ON login_sessions
		BEGIN SELECT RAISE(ABORT, 'fixture failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteAccount(ctx, admin, owner); err == nil {
		t.Fatal("expected deletion to roll back")
	}
	assertRows := func(expected int64) {
		t.Helper()
		for _, model := range ownedAccountTables {
			var count int64
			err := db.Model(model).Where("owner_id = ?", owner).Count(&count).Error
			if err != nil || count != expected {
				t.Fatalf("%s has %d rows, expected %d: %v", model.TableName(), count, expected, err)
			}
		}
	}
	assertRows(1)
	if err := db.Exec("DROP TRIGGER fail_delete").Error; err != nil {
		t.Fatal(err)
	}
	if err := repo.DeleteAccount(ctx, admin, owner); err != nil {
		t.Fatal(err)
	}
	assertRows(0)
	for _, item := range []any{&userRow{ID: admin}, &projectRow{ID: "other-project"}, &instanceSettingsRow{ID: instanceSettingsID}} {
		if err := db.First(item).Error; err != nil {
			t.Fatalf("unrelated %T disappeared: %v", item, err)
		}
	}
	if err := db.Create(&modelSettingsRow{OwnerID: owner, Data: data}).Error; err == nil {
		t.Fatal("late write recreated settings for a deleted account")
	}
	if _, err := repo.SaveE2BSandbox(ctx, owner, "project", "fixture", "", "new-box", data); err == nil {
		t.Fatal("late workspace creation resurrected a deleted account")
	}
}
