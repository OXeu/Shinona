package sqlite

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/auth"
)

func (r *Repository) HasOAuthIdentity(ctx context.Context, owner string) (bool, error) {
	return hasOAuthIdentity(r.db.WithContext(ctx), owner)
}

func hasOAuthIdentity(db *gorm.DB, owner string) (bool, error) {
	var binding identityRow
	err := db.Select("owner_id").Take(&binding, "owner_id = ?", owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

func (r *Repository) LinkOAuthIdentity(ctx context.Context, owner, sessionKey string, identity auth.Identity, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := accountCredentials(tx, owner, sessionKey, now); err != nil {
			return err
		}
		hasIdentity, err := hasOAuthIdentity(tx, owner)
		if err != nil {
			return err
		}
		if hasIdentity {
			return auth.ErrOAuthLinked
		}
		var existing identityRow
		err = tx.Select("owner_id").Take(&existing, "issuer = ? AND subject = ?", identity.Issuer, identity.Subject).Error
		if err == nil {
			return auth.ErrOAuthInUse
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err := tx.Create(&identityRow{Issuer: identity.Issuer, Subject: identity.Subject, OwnerID: owner}).Error; err != nil {
			return err
		}
		return tx.Model(&userRow{}).Where("id = ?", owner).Update("avatar_url", identity.AvatarURL).Error
	})
}

func (r *Repository) UnlinkOAuthIdentity(ctx context.Context, state auth.CredentialState, sessionKey string, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureCredentials(tx, state, sessionKey, now); err != nil {
			return err
		}
		if len(state.PasswordHash) == 0 || state.User.LoginEmail == "" {
			return auth.ErrOAuthPassword
		}
		owner := state.User.ID
		result := tx.Delete(&identityRow{}, "owner_id = ?", owner)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		if err := tx.Model(&userRow{}).Where("id = ?", owner).Update("avatar_url", "").Error; err != nil {
			return err
		}
		if err := tx.Delete(&flowRow{}, "owner_id = ?", owner).Error; err != nil {
			return err
		}
		return revokeOtherSessions(tx, owner, sessionKey)
	})
}
