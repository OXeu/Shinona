package sqlite

import (
	"context"
	"errors"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
)

type ProjectStore struct{ pools }

type projectRow struct {
	ID      string `gorm:"primaryKey"`
	OwnerID string `gorm:"not null;uniqueIndex:idx_projects_owner_name"`
	Name    string `gorm:"not null;uniqueIndex:idx_projects_owner_name"`
}

func (projectRow) TableName() string { return "projects" }

func (r *ProjectStore) CreateProject(ctx context.Context, project run.Project) error {
	record := projectRow(project)
	result := r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_id"}, {Name: "name"}}, DoNothing: true,
	}).Create(&record)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return run.ErrProjectExists
	}
	return nil
}

func (r *ProjectStore) Projects(ctx context.Context, ownerID string) ([]run.Project, error) {
	return listProjects(r.read.WithContext(ctx), ownerID)
}

func listProjects(db *gorm.DB, ownerID string) ([]run.Project, error) {
	var records []projectRow
	if err := db.Where("owner_id = ?", ownerID).Order("name COLLATE NOCASE, id").Find(&records).Error; err != nil {
		return nil, err
	}
	projects := make([]run.Project, len(records))
	for i, record := range records {
		projects[i] = run.Project(record)
	}
	return projects, nil
}

func (r *ProjectStore) Project(ctx context.Context, ownerID, id string) (run.Project, error) {
	var record projectRow
	err := r.read.WithContext(ctx).Take(&record, "owner_id = ? AND id = ?", ownerID, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return run.Project{}, run.ErrProject
	}
	return run.Project(record), err
}

func (r *ProjectStore) RenameProject(ctx context.Context, ownerID, id, name string) (run.Project, error) {
	var project projectRow
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		err := tx.Take(&project, "owner_id = ? AND id = ?", ownerID, id).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return run.ErrProject
		}
		if err != nil {
			return err
		}
		if project.Name == name {
			return nil
		}
		var count int64
		if err := tx.Model(&projectRow{}).Where("owner_id = ? AND name = ?", ownerID, name).Count(&count).Error; err != nil {
			return err
		}
		if count > 0 {
			return run.ErrProjectExists
		}
		project.Name = name
		return tx.Model(&project).UpdateColumn("name", name).Error
	})
	return run.Project(project), err
}

func (r *ProjectStore) RemoveProject(ctx context.Context, ownerID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// The current provider may only have cleaned up one control plane.
		// Keep any remaining binding until its remote sandbox can be removed.
		var retained bool
		if err := tx.Raw(`SELECT EXISTS (SELECT 1 FROM e2b_project_workspaces WHERE owner_id = ? AND project_id = ?)`, ownerID, id).
			Row().Scan(&retained); err != nil {
			return err
		}
		if retained {
			return run.ErrProjectCleanupRequired
		}
		now := time.Now().UTC()
		sessions := func() *gorm.DB {
			return tx.Model(&sessionRow{}).Where("owner_id = ? AND project_id = ?", ownerID, id)
		}
		queued := func() *gorm.DB {
			return tx.Model(&row{}).Where("owner_id = ? AND status = ?", ownerID, run.Queued)
		}
		// Stop turns submitted for this workspace before their sessions move.
		if err := sessions().Where("updated_at < ? AND id IN (?)", now, queued().Select("session_id")).
			UpdateColumn("updated_at", now).Error; err != nil {
			return err
		}
		if err := queued().Where("session_id IN (?)", sessions().Select("id")).
			UpdateColumns(map[string]any{"status": run.Stopped, "updated_at": now}).Error; err != nil {
			return err
		}
		if err := sessions().UpdateColumn("project_id", "").Error; err != nil {
			return err
		}
		result := tx.Delete(&projectRow{}, "owner_id = ? AND id = ?", ownerID, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return run.ErrProject
		}
		return nil
	})
}
