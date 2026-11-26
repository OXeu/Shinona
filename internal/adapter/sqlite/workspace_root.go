package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
)

// workspaceRootRow stores the directory a session uses for its workspace panel.
// An empty root is not stored and selects the project workspace root.
type workspaceRootRow struct {
	OwnerID   string `gorm:"primaryKey;not null"`
	SessionID string `gorm:"primaryKey;not null"`
	Root      string `gorm:"not null"`
}

func (workspaceRootRow) TableName() string { return "workspace_roots" }

func (r *Repository) WorkspaceRoot(ctx context.Context, ownerID, sessionID string) (string, error) {
	db := r.db.WithContext(ctx)
	if err := sessionScope(db, ownerID).Select("id").Take(&sessionRow{}, "id = ?", sessionID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", run.ErrSession
		}
		return "", err
	}
	var record workspaceRootRow
	err := db.Select("root").Take(&record, "owner_id = ? AND session_id = ?", ownerID, sessionID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil
	}
	return record.Root, err
}

func (r *Repository) SetWorkspaceRoot(ctx context.Context, ownerID, sessionID, root string) error {
	db := r.db.WithContext(ctx)
	if err := sessionScope(db, ownerID).Select("id").Take(&sessionRow{}, "id = ?", sessionID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run.ErrSession
		}
		return err
	}
	if root == "" {
		return db.Delete(&workspaceRootRow{}, "owner_id = ? AND session_id = ?", ownerID, sessionID).Error
	}
	return db.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "owner_id"}, {Name: "session_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"root"}),
	}).Create(&workspaceRootRow{OwnerID: ownerID, SessionID: sessionID, Root: root}).Error
}
