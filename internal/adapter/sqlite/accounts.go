package sqlite

import (
	"bytes"
	"context"
	"crypto/subtle"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/auth"
)

type avatarRow struct {
	OwnerID   string `gorm:"primaryKey"`
	Version   string
	MediaType string
	Data      []byte
}

func (avatarRow) TableName() string { return "account_avatars" }

type emailChallengeRow struct {
	ID         string
	OwnerID    string `gorm:"primaryKey"`
	SessionKey string
	Email      string
	CodeHash   string
	SentAt     time.Time
	ExpiresAt  time.Time `gorm:"index"`
	Attempts   int
}

func (emailChallengeRow) TableName() string { return "account_email_challenges" }

func (r *AccountStore) UpdateProfile(ctx context.Context, owner string, change auth.ProfileChange) (auth.User, error) {
	done := r.sessions.change("", owner)
	defer done()
	var row userRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := readEnabledAccount(tx, owner, &row); err != nil {
			return err
		}
		row.Name = change.Name
		if change.Avatar != nil {
			row.AvatarVersion = change.Avatar.Version
			if len(change.Avatar.Data) > 0 {
				avatar := avatarRow{OwnerID: owner, Version: row.AvatarVersion, MediaType: change.Avatar.MediaType, Data: change.Avatar.Data}
				if err := tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&avatar).Error; err != nil {
					return err
				}
			} else if err := tx.Delete(&avatarRow{}, "owner_id = ?", owner).Error; err != nil {
				return err
			}
		}
		return tx.Model(&row).Select("name", "avatar_version").Updates(&row).Error
	})
	return row.user(), err
}

func (r *AccountStore) AccountAvatar(ctx context.Context, owner, version string) (*auth.Avatar, error) {
	var row avatarRow
	err := r.read.WithContext(ctx).First(&row, "owner_id = ? AND version = ?", owner, version).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, auth.ErrAccountMissing
	}
	if err != nil {
		return nil, err
	}
	return &auth.Avatar{Version: row.Version, MediaType: row.MediaType, Data: row.Data}, nil
}

func (r *AccountStore) AccountCredentials(ctx context.Context, owner, sessionKey string, now time.Time) (auth.CredentialState, error) {
	return accountCredentials(r.read.WithContext(ctx), owner, sessionKey, now)
}

func accountCredentials(db *gorm.DB, owner, sessionKey string, now time.Time) (auth.CredentialState, error) {
	var entry struct {
		Account         userRow `gorm:"embedded"`
		AuthenticatedAt time.Time
	}
	err := db.Model(&userRow{}).Select("users.*, login_sessions.authenticated_at").
		Joins("JOIN login_sessions ON login_sessions.owner_id = users.id").
		Take(&entry, "users.id = ? AND users.disabled = ? AND login_sessions.key = ? AND login_sessions.expires_at > ?",
			owner, false, sessionKey, now).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return auth.CredentialState{}, auth.ErrRecentSignIn
	}
	return auth.CredentialState{User: entry.Account.user(), PasswordHash: entry.Account.PasswordHash, AuthenticatedAt: entry.AuthenticatedAt}, err
}

func ensureCredentials(tx *gorm.DB, state auth.CredentialState, sessionKey string, now time.Time) error {
	current, err := accountCredentials(tx, state.User.ID, sessionKey, now)
	if err != nil {
		return err
	}
	if current.User.LoginEmail != state.User.LoginEmail || !bytes.Equal(current.PasswordHash, state.PasswordHash) {
		return auth.ErrAccountChanged
	}
	return nil
}

func (r *AccountStore) ChangePassword(ctx context.Context, state auth.CredentialState, sessionKey string, hash []byte, now time.Time) error {
	done := r.sessions.change("", state.User.ID)
	defer done()
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureCredentials(tx, state, sessionKey, now); err != nil {
			return err
		}
		owner := state.User.ID
		if err := tx.Model(&userRow{}).Where("id = ?", owner).Update("password_hash", hash).Error; err != nil {
			return err
		}
		if err := tx.Delete(&emailChallengeRow{}, "owner_id = ?", owner).Error; err != nil {
			return err
		}
		return revokeOtherSessions(tx, owner, sessionKey)
	})
	return err
}

func (r *AccountStore) SaveEmailChallenge(ctx context.Context, state auth.CredentialState, input auth.EmailChallenge) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureCredentials(tx, state, input.SessionKey, input.SentAt); err != nil {
			return err
		}
		if err := tx.Delete(&emailChallengeRow{}, "expires_at <= ?", input.SentAt).Error; err != nil {
			return err
		}
		var existing int64
		if err := tx.Model(&emailChallengeRow{}).Where("owner_id = ? AND sent_at > ?", input.OwnerID,
			input.SentAt.Add(-auth.EmailCooldown)).Count(&existing).Error; err != nil {
			return err
		}
		if existing != 0 {
			return auth.ErrEmailWait
		}
		if err := ensureEmailAvailable(tx, input.Email); err != nil {
			return err
		}
		row := emailChallengeRow(input)
		return tx.Clauses(clause.OnConflict{UpdateAll: true}).Create(&row).Error
	})
}

func (r *AccountStore) VerifyEmailChallenge(ctx context.Context, owner, sessionKey, id, codeHash string, now time.Time) error {
	done := r.sessions.change("", owner)
	defer done()
	var rejected error
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := accountCredentials(tx, owner, sessionKey, now); err != nil {
			return err
		}
		var challenge emailChallengeRow
		err := tx.Take(&challenge, "owner_id = ? AND id = ? AND session_key = ? AND expires_at > ? AND attempts < ?",
			owner, id, sessionKey, now, auth.MaxCodeAttempts).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return auth.ErrEmailCode
		}
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(challenge.CodeHash), []byte(codeHash)) != 1 {
			rejected = auth.ErrEmailCode
			return tx.Model(&challenge).Update("attempts", challenge.Attempts+1).Error
		}
		if err := ensureEmailAvailable(tx, challenge.Email); err != nil {
			return err
		}
		if err := tx.Model(&userRow{}).Where("id = ?", owner).
			Updates(map[string]any{"email": challenge.Email, "login": challenge.Email}).Error; err != nil {
			return err
		}
		if err := revokeOtherSessions(tx, owner, sessionKey); err != nil {
			return err
		}
		return tx.Delete(&challenge).Error
	})
	if err != nil {
		return err
	}
	return rejected
}

func (r *AccountStore) ListAccounts(ctx context.Context, search string, offset, limit int) (auth.AccountPage, error) {
	page := auth.AccountPage{Users: make([]auth.User, 0)}
	err := r.read.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		query := tx.Model(&userRow{})
		if search != "" {
			escaped := strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(search)
			pattern := "%" + escaped + "%"
			query = query.Where(`email LIKE ? ESCAPE '\' OR name LIKE ? ESCAPE '\' OR id = ?`, pattern, pattern, search)
		}
		if err := query.Count(&page.Total).Error; err != nil {
			return err
		}
		var rows []userRow
		if err := query.Order("email, id").Offset(offset).Limit(limit).Find(&rows).Error; err != nil {
			return err
		}
		for _, row := range rows {
			page.Users = append(page.Users, row.user())
		}
		return nil
	})
	return page, err
}

func (r *AccountStore) CreateManagedAccount(ctx context.Context, actor string, user auth.User, hash []byte) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureAdmin(tx, actor); err != nil {
			return err
		}
		return createPasswordAccount(tx, user, hash)
	})
}

func (r *AccountStore) SetAccountAccess(ctx context.Context, actor, target string, access auth.AccountAccess) (auth.User, error) {
	done := r.sessions.change("", target)
	defer done()
	var row userRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := ensureAdmin(tx, actor); err != nil {
			return err
		}
		if err := tx.Take(&row, "id = ?", target).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return auth.ErrAccountMissing
			}
			return err
		}
		if access.Admin != nil {
			row.Admin = *access.Admin
		}
		if access.Disabled != nil {
			row.Disabled = *access.Disabled
		}
		if err := tx.Model(&row).Select("admin", "disabled").Updates(&row).Error; err != nil {
			return err
		}
		if row.Disabled {
			return revokeOtherSessions(tx, row.ID, "")
		}
		return nil
	})
	return row.user(), err
}

func createPasswordAccount(tx *gorm.DB, user auth.User, hash []byte) error {
	if err := ensureEmailAvailable(tx, user.Email); err != nil {
		return err
	}
	return tx.Create(&userRow{ID: user.ID, Email: user.Email, Name: user.Name, Login: &user.Email, PasswordHash: hash}).Error
}

func ensureEmailAvailable(tx *gorm.DB, email string) error {
	var count int64
	if err := tx.Model(&userRow{}).Where("login = ?", email).Count(&count).Error; err != nil {
		return err
	}
	if count != 0 {
		return auth.ErrEmailInUse
	}
	return nil
}

func readEnabledAccount(tx *gorm.DB, owner string, row *userRow) error {
	err := tx.Take(row, "id = ? AND disabled = ?", owner, false).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return auth.ErrAccountMissing
	}
	return err
}

func ensureAdmin(tx *gorm.DB, actor string) error {
	var count int64
	if err := tx.Model(&userRow{}).Where("id = ? AND admin = ? AND disabled = ?", actor, true, false).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		return auth.ErrAdminRequired
	}
	return nil
}

func revokeOtherSessions(tx *gorm.DB, owner, keep string) error {
	// A revoked session also loses any outstanding email verification.
	if err := tx.Delete(&emailChallengeRow{}, "owner_id = ? AND session_key <> ?", owner, keep).Error; err != nil {
		return err
	}
	return tx.Delete(&loginRow{}, "owner_id = ? AND key <> ?", owner, keep).Error
}
