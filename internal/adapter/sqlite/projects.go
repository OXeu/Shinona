package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
)

type projectRow struct {
	ID      string `gorm:"primaryKey"`
	OwnerID string `gorm:"not null;uniqueIndex:idx_projects_owner_name"`
	Name    string `gorm:"not null;uniqueIndex:idx_projects_owner_name"`
}

func (projectRow) TableName() string { return "projects" }

func (r *Repository) CreateProject(ctx context.Context, project run.Project) error {
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

func (r *Repository) Projects(ctx context.Context, ownerID string) ([]run.Project, error) {
	var records []projectRow
	if err := r.read.WithContext(ctx).Where("owner_id = ?", ownerID).Order("name COLLATE NOCASE, id").Find(&records).Error; err != nil {
		return nil, err
	}
	projects := make([]run.Project, len(records))
	for i, record := range records {
		projects[i] = run.Project(record)
	}
	return projects, nil
}

func (r *Repository) Project(ctx context.Context, ownerID, id string) (run.Project, error) {
	var record projectRow
	err := r.read.WithContext(ctx).Take(&record, "owner_id = ? AND id = ?", ownerID, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return run.Project{}, run.ErrProject
	}
	return run.Project(record), err
}

func (r *Repository) RenameProject(ctx context.Context, ownerID, id, name string) (run.Project, error) {
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

func (r *Repository) RemoveProject(ctx context.Context, ownerID, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Delete(&projectRow{}, "owner_id = ? AND id = ?", ownerID, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return run.ErrProject
		}
		if err := tx.Delete(&projectSandboxRow{}, "owner_id = ? AND project_id = ?", ownerID, id).Error; err != nil {
			return err
		}
		return tx.Model(&sessionRow{}).Where("owner_id = ? AND project_id = ?", ownerID, id).
			UpdateColumn("project_id", "").Error
	})
}
