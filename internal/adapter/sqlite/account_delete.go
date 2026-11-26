package sqlite

import (
	"context"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/auth"
)

func (r *AccountStore) DeleteAccount(ctx context.Context, actor, target string) error {
	done := r.sessions.change("", target)
	defer done()
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
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
		return nil
	})
}
