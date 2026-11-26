package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
)

type legacyWorkspaceRootRow struct {
	OwnerID   string `gorm:"primaryKey;not null"`
	SessionID string `gorm:"primaryKey;not null"`
	Root      string `gorm:"not null"`
}

func (legacyWorkspaceRootRow) TableName() string { return "workspace_roots" }

type workspaceRootRow struct {
	OwnerID   string `gorm:"primaryKey;not null"`
	ProjectID string `gorm:"primaryKey;not null"`
	Root      string `gorm:"not null"`
}

func (workspaceRootRow) TableName() string { return "project_workspace_roots" }

func migrateWorkspaceRoots(db *gorm.DB) error {
	return db.Transaction(func(tx *gorm.DB) error {
		// Creating and populating the table in one transaction makes this a
		// one-time migration, including when the user later clears a choice.
		migrator := tx.Migrator()
		if migrator.HasTable(&workspaceRootRow{}) {
			return nil
		}
		if err := migrator.CreateTable(&workspaceRootRow{}); err != nil {
			return err
		}
		if !migrator.HasTable(&legacyWorkspaceRootRow{}) || !migrator.HasTable(&sessionRow{}) {
			return nil
		}
		// Legacy preferences had no timestamp. Choose the most recently updated
		// non-archived session with a saved directory in each project.
		return tx.Exec(`INSERT INTO project_workspace_roots (owner_id, project_id, root)
			SELECT owner_id, project_id, root FROM (
				SELECT s.owner_id, s.project_id, w.root,
					ROW_NUMBER() OVER (PARTITION BY s.owner_id, s.project_id ORDER BY s.updated_at DESC, s.id DESC) AS preference
				FROM workspace_roots AS w
				JOIN agent_sessions AS s ON s.owner_id = w.owner_id AND s.id = w.session_id
				WHERE s.archived_at IS NULL AND w.root <> ''
			) WHERE preference = 1`).Error
	})
}

func (r *Repository) WorkspaceRoot(ctx context.Context, ownerID, sessionID string) (string, error) {
	var record workspaceRootRow
	err := r.read.WithContext(ctx).Model(&sessionRow{}).
		Select("COALESCE(roots.root, '') AS root").
		Joins("LEFT JOIN project_workspace_roots AS roots ON roots.owner_id = agent_sessions.owner_id AND roots.project_id = agent_sessions.project_id").
		Where("agent_sessions.owner_id = ? AND agent_sessions.id = ? AND agent_sessions.archived_at IS NULL", ownerID, sessionID).
		Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", run.ErrSession
	}
	return record.Root, err
}

func (r *Repository) SetWorkspaceRoot(ctx context.Context, ownerID, sessionID, projectID, root string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := sessionProject(tx, ownerID, sessionID)
		if err != nil {
			return err
		}
		if current != projectID {
			return run.ErrWorkspaceBusy
		}
		if root == "" {
			return tx.Delete(&workspaceRootRow{}, "owner_id = ? AND project_id = ?", ownerID, projectID).Error
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "owner_id"}, {Name: "project_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"root"}),
		}).Create(&workspaceRootRow{OwnerID: ownerID, ProjectID: projectID, Root: root}).Error
	})
}
