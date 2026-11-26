package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

// Migrations are applied in order under the writer transaction. An unversioned
// database may be empty or contain the pre-migration Shinona schema.
var migrations = [...]func(*gorm.DB) error{
	initializeSchema, replaceLegacyTriggers, migrateRunAttachments, migrateAuthenticationTime, deleteRevokedMCPGrants,
	migrateOwnership, migrateSettingsStorage, addInstanceSettingsRevision,
	initSessionListRevisions,
}

func addInstanceSettingsRevision(db *gorm.DB) error {
	return db.Exec(`CREATE TABLE instance_settings_revision (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		revision INTEGER NOT NULL CHECK (revision >= 0)
	); INSERT INTO instance_settings_revision (id, revision) VALUES (1, 0)`).Error
}

func migrate(db *gorm.DB) error {
	return migrateTo(db, len(migrations))
}

func migrateTo(db *gorm.DB, target int) error {
	version, err := schemaVersion(db)
	if err != nil || version >= target {
		return err
	}
	return db.Connection(func(conn *gorm.DB) (err error) {
		// SQLite's table rebuild requires a single connection with enforcement
		// disabled before the transaction. The temporary table's rename keeps
		// existing views and triggers pointing to the original table name.
		defer func() {
			restore := conn.WithContext(context.WithoutCancel(conn.Statement.Context))
			err = errors.Join(err, restore.Exec("PRAGMA foreign_keys = ON; PRAGMA legacy_alter_table = OFF").Error)
		}()
		if err := conn.Exec("PRAGMA foreign_keys = OFF; PRAGMA legacy_alter_table = ON").Error; err != nil {
			return err
		}
		return conn.Transaction(func(tx *gorm.DB) error {
			// Another process may have migrated while we waited for the writer.
			version, err := schemaVersion(tx)
			if err != nil {
				return err
			}
			for i := version; i < target; i++ {
				if err := migrations[i](tx); err != nil {
					return fmt.Errorf("SQLite migration %d: %w", i+1, err)
				}
				if err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)).Error; err != nil {
					return err
				}
			}
			return checkForeignKeys(tx)
		})
	})
}

func checkForeignKeys(db *gorm.DB) error {
	var table, parent string
	err := db.Raw(`SELECT "table", parent FROM pragma_foreign_key_check LIMIT 1`).Row().Scan(&table, &parent)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return nil
	case err != nil:
		return err
	default:
		return fmt.Errorf("SQLite foreign key check failed: %s references a missing row in %s", table, parent)
	}
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
		&userRow{}, &legacyInstanceSettingsRow{},
		&row{}, &workspaceRootRow{}, &legacyWorkspaceRootRow{}, &sessionRow{}, &projectRow{},
		&settingsRow{}, &composerDefaultsRow{}, &legacyModelSettingsRow{}, &legacyMCPSettingsRow{}, &mcpGrantRow{},
		&e2bWorkspaceRow{}, &projectSandboxRow{}, &sshServerRow{},
		&avatarRow{}, &emailChallengeRow{}, &identityRow{}, &loginRow{}, &flowRow{},
	); err != nil {
		return err
	}
	if err := db.Exec("CREATE TABLE IF NOT EXISTS deleted_accounts (id TEXT PRIMARY KEY)").Error; err != nil {
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
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS session_list_revisions (
		owner_id TEXT PRIMARY KEY, revision INTEGER NOT NULL
	) WITHOUT ROWID`).Error; err != nil {
		return err
	}
	if err := initSessionListRevisions(db); err != nil {
		return err
	}
	return db.Exec(`DELETE FROM session_list_revisions
		WHERE owner_id IN (SELECT id FROM deleted_accounts)`).Error
}

func migrateAuthenticationTime(db *gorm.DB) error {
	if migrator := db.Migrator(); !migrator.HasColumn(&loginRow{}, "AuthenticatedAt") {
		if err := migrator.AddColumn(&loginRow{}, "AuthenticatedAt"); err != nil {
			return err
		}
	}
	// Prior releases recorded a fixed expiry instead of the authentication time.
	modifier := fmt.Sprintf("-%d seconds", auth.SessionLifetime/time.Second)
	return db.Exec(`UPDATE login_sessions SET authenticated_at = strftime('%Y-%m-%d %H:%M:%f', expires_at, ?)
		WHERE authenticated_at IS NULL`, modifier).Error
}

func deleteRevokedMCPGrants(db *gorm.DB) error {
	return db.Exec(`DELETE FROM mcp_oauth_grants WHERE grant = ?`, []byte("{}")).Error
}
