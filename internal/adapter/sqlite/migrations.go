package sqlite

import (
	"fmt"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

// Migrations are applied in order under the writer transaction. An unversioned
// database may be empty or contain the pre-migration Shinona schema.
var migrations = [...]func(*gorm.DB) error{initializeSchema, replaceLegacyTriggers, migrateRunAttachments}

func migrate(db *gorm.DB) error {
	version, err := schemaVersion(db)
	if err != nil || version == len(migrations) {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		// Another process may have migrated while we waited for the writer.
		version, err := schemaVersion(tx)
		if err != nil {
			return err
		}
		for i := version; i < len(migrations); i++ {
			if err := migrations[i](tx); err != nil {
				return fmt.Errorf("SQLite migration %d: %w", i+1, err)
			}
			if err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func schemaVersion(db *gorm.DB) (int, error) {
	var version int
	if err := db.Raw("PRAGMA user_version").Row().Scan(&version); err != nil {
		return 0, err
	}
	if version < 0 || version > len(migrations) {
		return 0, fmt.Errorf("unsupported SQLite schema version %d", version)
	}
	return version, nil
}

func initializeSchema(db *gorm.DB) error {
	if err := migrateWorkspaceRoots(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(
		&userRow{}, &instanceSettingsRow{}, &deletedAccountRow{},
		&row{}, &workspaceRootRow{}, &legacyWorkspaceRootRow{}, &sessionRow{}, &projectRow{},
		&settingsRow{}, &composerDefaultsRow{}, &modelSettingsRow{}, &mcpSettingsRow{}, &mcpGrantRow{},
		&e2bWorkspaceRow{}, &projectSandboxRow{}, &sshServerRow{},
		&avatarRow{}, &emailChallengeRow{}, &identityRow{}, &loginRow{}, &flowRow{},
	); err != nil {
		return err
	}
	if err := db.Exec("DROP INDEX IF EXISTS idx_runs_session_status").Error; err != nil {
		return err
	}
	if err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_runs_executing_project
		ON runs (owner_id, session_id) WHERE status = 'RUNNING'`).Error; err != nil {
		return err
	}
	if err := retainE2BProjectProviders(db); err != nil {
		return err
	}
	return db.Model(&row{}).Where("backend = ? AND policy = ? AND status = ?", sandbox.ProviderE2B, sandbox.Fresh, run.Queued).
		UpdateColumn("policy", sandbox.Shared).Error
}

func replaceLegacyTriggers(db *gorm.DB) error {
	for _, table := range []string{"users", "login_sessions"} {
		for _, action := range []string{"INSERT", "UPDATE", "DELETE"} {
			if err := db.Exec(fmt.Sprintf("DROP TRIGGER IF EXISTS session_auth_%s_%s", table, action)).Error; err != nil {
				return err
			}
		}
	}
	if err := db.Exec("DROP TABLE IF EXISTS session_auth_revision").Error; err != nil {
		return err
	}
	if err := protectDeletedAccounts(db); err != nil {
		return err
	}
	if err := initSessionListRevisions(db); err != nil {
		return err
	}
	return db.Exec(`DELETE FROM session_list_revisions
		WHERE owner_id IN (SELECT id FROM deleted_accounts)`).Error
}
