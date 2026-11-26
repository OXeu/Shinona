package sqlite

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/auth"
)

type userRow struct {
	ID            string `gorm:"primaryKey"`
	Email         string `gorm:"not null"`
	Name          string
	AvatarURL     string
	AvatarVersion string
	Login         *string `gorm:"uniqueIndex"`
	PasswordHash  []byte
	Admin         bool
	Disabled      bool `gorm:"not null;default:false"`
}

func (userRow) TableName() string { return "users" }

func (row userRow) user() auth.User {
	user := auth.User{ID: row.ID, Email: row.Email, Name: row.Name, AvatarURL: row.AvatarURL,
		HasPassword: len(row.PasswordHash) > 0, Admin: row.Admin, Disabled: row.Disabled}
	if row.Login != nil {
		user.LoginEmail = *row.Login
	}
	if row.AvatarVersion != "" {
		user.HasAvatar = true
		user.AvatarURL = "/auth/account/avatar/" + row.ID + "/" + row.AvatarVersion
	}
	return user
}

type identityRow struct {
	Issuer  string `gorm:"primaryKey"`
	Subject string `gorm:"primaryKey"`
	OwnerID string `gorm:"not null;index"`
}

func (identityRow) TableName() string { return "oauth_identities" }

type loginRow struct {
	Key       string    `gorm:"primaryKey"`
	OwnerID   string    `gorm:"not null;index"`
	ExpiresAt time.Time `gorm:"not null;index"`
}

func (loginRow) TableName() string { return "login_sessions" }

type flowRow struct {
	Key        string    `gorm:"primaryKey"`
	Nonce      string    `gorm:"not null"`
	Verifier   string    `gorm:"not null"`
	ExpiresAt  time.Time `gorm:"not null;index"`
	Version    string
	OwnerID    string `gorm:"index"`
	SessionKey string
}

func (flowRow) TableName() string { return "oauth_flows" }

func (r *Repository) CreatePasswordUser(ctx context.Context, user auth.User, hash []byte) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return createPasswordAccount(tx, user, hash)
	})
}

func (r *Repository) PasswordUser(ctx context.Context, email string) (auth.User, []byte, error) {
	var row userRow
	err := r.db.WithContext(ctx).First(&row, "login = ?", email).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return auth.User{}, nil, nil
	}
	return row.user(), row.PasswordHash, err
}

func (r *Repository) OAuthUser(ctx context.Context, identity auth.Identity, id string) (auth.User, error) {
	var user userRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var record identityRow
		err := tx.First(&record, "issuer = ? AND subject = ?", identity.Issuer, identity.Subject).Error
		if err == nil {
			if err := tx.First(&user, "id = ?", record.OwnerID).Error; err != nil {
				return err
			}
			if user.Disabled || user.AvatarURL == identity.AvatarURL {
				return nil
			}
			user.AvatarURL = identity.AvatarURL
			return tx.Model(&user).Update("avatar_url", user.AvatarURL).Error
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		user = userRow{ID: id, Email: identity.Email, Name: identity.Name, AvatarURL: identity.AvatarURL}
		if err := tx.Create(&user).Error; err != nil {
			return err
		}
		return tx.Create(&identityRow{Issuer: identity.Issuer, Subject: identity.Subject, OwnerID: id}).Error
	})
	return user.user(), err
}

func (r *Repository) SaveSession(ctx context.Context, key, ownerID string, expiresAt time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		return saveSession(tx, key, ownerID, expiresAt)
	})
}

func (r *Repository) SaveOAuthSession(ctx context.Context, key, ownerID string, identity auth.Identity, expiresAt time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var binding identityRow
		err := tx.Select("owner_id").Take(&binding, "issuer = ? AND subject = ? AND owner_id = ?", identity.Issuer, identity.Subject, ownerID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.ErrCredentials
		}
		if err != nil {
			return err
		}
		return saveSession(tx, key, ownerID, expiresAt)
	})
}

func saveSession(tx *gorm.DB, key, ownerID string, expiresAt time.Time) error {
	var account userRow
	if err := readEnabledAccount(tx, ownerID, &account); err != nil {
		return err
	}
	if err := tx.Where("expires_at <= ?", time.Now().UTC()).Delete(&loginRow{}).Error; err != nil {
		return err
	}
	return tx.Create(&loginRow{Key: key, OwnerID: ownerID, ExpiresAt: expiresAt}).Error
}

func (r *Repository) SessionUser(ctx context.Context, key string, now time.Time) (*auth.User, error) {
	var user userRow
	err := r.db.WithContext(ctx).
		Joins("JOIN login_sessions ON login_sessions.owner_id = users.id").
		First(&user, "login_sessions.key = ? AND login_sessions.expires_at > ? AND users.disabled = ?", key, now, false).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	account := user.user()
	return &account, nil
}

func (r *Repository) DeleteSession(ctx context.Context, key string) error {
	return r.db.WithContext(ctx).Delete(&loginRow{}, "key = ?", key).Error
}

func (r *Repository) SaveFlow(ctx context.Context, flow auth.Flow, now time.Time) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("expires_at <= ?", now).Delete(&flowRow{}).Error; err != nil {
			return err
		}
		record := flowRow(flow)
		return tx.Create(&record).Error
	})
}

func (r *Repository) ConsumeFlow(ctx context.Context, key string, now time.Time) (*auth.Flow, error) {
	var records []flowRow
	err := r.db.WithContext(ctx).Clauses(clause.Returning{}).
		Where("key = ? AND expires_at > ?", key, now).Delete(&records).Error
	if err != nil || len(records) == 0 {
		return nil, err
	}
	flow := auth.Flow(records[0])
	return &flow, nil
}

var _ auth.Repository = (*Repository)(nil)

// SetAdmin changes privileges for an existing account selected by its ID.
func (r *Repository) SetAdmin(ctx context.Context, id string, admin bool) error {
	result := r.db.WithContext(ctx).Model(&userRow{}).Where("id = ?", id).Update("admin", admin)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("account not found")
	}
	return nil
}
