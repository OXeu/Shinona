package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
)

type projectSandboxRow struct {
	OwnerID                  string `gorm:"primaryKey;not null"`
	ProjectID                string `gorm:"primaryKey;not null"`
	run.ProjectSandboxConfig `gorm:"embedded"`
}

func (projectSandboxRow) TableName() string { return "project_sandbox_settings" }

func (r *Repository) ProjectSandboxConfig(ctx context.Context, ownerID, projectID string) (*run.ProjectSandboxConfig, error) {
	var record projectSandboxRow
	err := r.db.WithContext(ctx).Take(&record, "owner_id = ? AND project_id = ?", ownerID, projectID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return &record.ProjectSandboxConfig, err
}

func (r *Repository) SaveProjectSandboxConfig(ctx context.Context, ownerID, projectID string, config run.ProjectSandboxConfig) error {
	return r.saveProjectSandboxConfig(ctx, ownerID, projectID, config, clause.OnConflict{
		Columns:   []clause.Column{{Name: "owner_id"}, {Name: "project_id"}},
		DoUpdates: clause.AssignmentColumns([]string{"provider", "template", "timeout_seconds", "on_timeout"}),
	})
}

func (r *Repository) RememberProjectSandboxProvider(ctx context.Context, ownerID, projectID, provider string) error {
	return r.saveProjectSandboxConfig(ctx, ownerID, projectID, run.ProjectSandboxConfig{Provider: provider},
		clause.OnConflict{DoNothing: true})
}

func (r *Repository) saveProjectSandboxConfig(ctx context.Context, ownerID, projectID string, config run.ProjectSandboxConfig, conflict clause.OnConflict) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if projectID != "" {
			var project projectRow
			err := tx.Take(&project, "owner_id = ? AND id = ?", ownerID, projectID).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return run.ErrProject
			}
			if err != nil {
				return err
			}
		}
		return tx.Clauses(conflict).Create(&projectSandboxRow{
			OwnerID: ownerID, ProjectID: projectID, ProjectSandboxConfig: config,
		}).Error
	})
}

func retainE2BProjectProviders(db *gorm.DB) error {
	return db.Exec(`INSERT INTO project_sandbox_settings (owner_id, project_id, provider, template, timeout_seconds, on_timeout)
		SELECT DISTINCT box.owner_id, box.project_id, 'e2b', '', 0, ''
		FROM e2b_project_workspaces AS box
		WHERE box.project_id = '' OR EXISTS (
			SELECT 1 FROM projects WHERE owner_id = box.owner_id AND id = box.project_id
		)
		ON CONFLICT (owner_id, project_id) DO NOTHING`).Error
}
