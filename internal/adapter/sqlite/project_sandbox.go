package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

type projectSandboxRow struct {
	OwnerID                  string `gorm:"primaryKey;not null"`
	ProjectID                string `gorm:"primaryKey;not null"`
	SSHServerID              *string
	run.ProjectSandboxConfig `gorm:"embedded"`
}

func (projectSandboxRow) TableName() string { return "project_sandbox_settings" }

func (r *ProjectStore) ProjectSandboxConfig(ctx context.Context, ownerID, projectID string) (*run.ProjectSandboxConfig, error) {
	return readProjectSandboxConfig(r.read.WithContext(ctx), ownerID, projectID)
}

func readProjectSandboxConfig(db *gorm.DB, ownerID, projectID string) (*run.ProjectSandboxConfig, error) {
	var record projectSandboxRow
	err := db.Take(&record, "owner_id = ? AND project_id = ?", ownerID, projectID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if record.SSHServerID != nil {
		record.Provider = settings.SSHProviderName(*record.SSHServerID)
	}
	return &record.ProjectSandboxConfig, nil
}

func (r *ProjectStore) SaveProjectSandboxConfig(ctx context.Context, ownerID, projectID string, config run.ProjectSandboxConfig) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := projectSandboxRecord(tx, ownerID, projectID, config)
		if err != nil {
			return err
		}
		return tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "owner_id"}, {Name: "project_id"}},
			DoUpdates: clause.AssignmentColumns([]string{"provider", "ssh_server_id", "template", "timeout_seconds", "on_timeout"}),
		}).Create(&record).Error
	})
}

func (r *ProjectStore) RememberProjectSandboxProvider(ctx context.Context, ownerID, projectID, provider string) (run.ProjectSandboxConfig, error) {
	var saved run.ProjectSandboxConfig
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		current, err := readProjectSandboxConfig(tx, ownerID, projectID)
		if err != nil {
			return err
		}
		if current != nil {
			saved = *current
			return nil
		}
		saved = run.ProjectSandboxConfig{Provider: provider}
		record, err := projectSandboxRecord(tx, ownerID, projectID, saved)
		if err != nil {
			return err
		}
		return tx.Create(&record).Error
	})
	return saved, err
}

func projectSandboxRecord(tx *gorm.DB, ownerID, projectID string, config run.ProjectSandboxConfig) (projectSandboxRow, error) {
	record := projectSandboxRow{OwnerID: ownerID, ProjectID: projectID, ProjectSandboxConfig: config}
	if id, personal := settings.SSHProviderServerID(config.Provider); personal {
		record.Provider = sandbox.ProviderSSH
		record.SSHServerID = &id
	}
	if record.SSHServerID != nil {
		if _, err := findSSHServer(tx.Select("id"), ownerID, *record.SSHServerID); err != nil {
			return projectSandboxRow{}, err
		}
	}
	if projectID != "" {
		var project projectRow
		err := tx.Take(&project, "owner_id = ? AND id = ?", ownerID, projectID).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return projectSandboxRow{}, run.ErrProject
		}
		if err != nil {
			return projectSandboxRow{}, err
		}
	}
	return record, nil
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
