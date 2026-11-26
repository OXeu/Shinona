package sqlite

import (
	"context"
	"fmt"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/auth"
)

type deletedAccountRow struct {
	ID string `gorm:"primaryKey"`
}

func (deletedAccountRow) TableName() string { return "deleted_accounts" }

var ownedAccountTables = []interface{ TableName() string }{
	&row{}, &workspaceRootRow{}, &legacyWorkspaceRootRow{}, &sessionRow{}, &projectRow{}, &settingsRow{}, &composerDefaultsRow{},
	&modelSettingsRow{}, &mcpSettingsRow{}, &mcpGrantRow{}, &e2bWorkspaceRow{}, &projectSandboxRow{}, &sshServerRow{},
	&avatarRow{}, &emailChallengeRow{}, &identityRow{}, &loginRow{}, &flowRow{},
}

func protectDeletedAccounts(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// An in-flight request can outlive its session. Prevent it from
		// recreating deleted rows, including when sandbox provisioning finishes late.
		for _, table := range ownedAccountTables {
			name := table.TableName()
			trigger := fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS reject_deleted_owner_%[1]s
				BEFORE INSERT ON %[1]s
				WHEN EXISTS (SELECT 1 FROM deleted_accounts WHERE id = NEW.owner_id)
				BEGIN SELECT RAISE(ABORT, 'account was deleted'); END`, name)
			if err := tx.Exec(trigger).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (r *Repository) DeleteAccount(ctx context.Context, actor, target string) error {
	done := r.sessions.change("", target)
	defer done()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureAdmin(tx, actor); err != nil {
			return err
		}
		if actor == target {
			return auth.ErrAdminSelf
		}
		result := tx.Delete(&userRow{}, "id = ?", target)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return auth.ErrAccountMissing
		}
		if err := tx.Create(&deletedAccountRow{ID: target}).Error; err != nil {
			return err
		}
		for _, table := range ownedAccountTables {
			if err := tx.Delete(table, "owner_id = ?", target).Error; err != nil {
				return fmt.Errorf("delete account rows from %s: %w", table.TableName(), err)
			}
		}
		return tx.Exec("DELETE FROM session_list_revisions WHERE owner_id = ?", target).Error
	})
	return err
}
