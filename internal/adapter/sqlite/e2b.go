package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type E2BStore struct{ pools }

type e2bWorkspaceRow struct {
	OwnerID     string `gorm:"primaryKey;not null"`
	ProjectID   string `gorm:"primaryKey;not null"`
	Endpoint    string `gorm:"primaryKey;not null"`
	SandboxID   string `gorm:"not null"`
	Credentials []byte `gorm:"not null"`
}

func (e2bWorkspaceRow) TableName() string { return "e2b_project_workspaces" }

func (r *E2BStore) E2BSandbox(ctx context.Context, ownerID, projectID, endpoint string) (string, []byte, error) {
	var record e2bWorkspaceRow
	err := r.read.WithContext(ctx).Select("sandbox_id", "credentials").Take(&record,
		"owner_id = ? AND project_id = ? AND endpoint = ?", ownerID, projectID, endpoint).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return "", nil, nil
	}
	return record.SandboxID, record.Credentials, err
}

func (r *E2BStore) SaveE2BSandbox(ctx context.Context, ownerID, projectID, endpoint, previousID, sandboxID string, credentials []byte) (bool, error) {
	db := r.db.WithContext(ctx)
	if previousID != "" {
		result := db.Model(&e2bWorkspaceRow{}).
			Where("owner_id = ? AND project_id = ? AND endpoint = ? AND sandbox_id = ?", ownerID, projectID, endpoint, previousID).
			UpdateColumns(map[string]any{"sandbox_id": sandboxID, "credentials": credentials})
		return result.RowsAffected == 1, result.Error
	}
	result := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&e2bWorkspaceRow{
		OwnerID: ownerID, ProjectID: projectID, Endpoint: endpoint, SandboxID: sandboxID, Credentials: credentials,
	})
	return result.RowsAffected == 1, result.Error
}

func (r *E2BStore) UpdateE2BCredentials(ctx context.Context, ownerID, projectID, endpoint, sandboxID string, credentials []byte) error {
	return r.db.WithContext(ctx).Model(&e2bWorkspaceRow{}).
		Where("owner_id = ? AND project_id = ? AND endpoint = ? AND sandbox_id = ?", ownerID, projectID, endpoint, sandboxID).
		UpdateColumn("credentials", credentials).Error
}

func (r *E2BStore) DeleteE2BSandbox(ctx context.Context, ownerID, projectID, endpoint, sandboxID string) error {
	return r.db.WithContext(ctx).Delete(&e2bWorkspaceRow{},
		"owner_id = ? AND project_id = ? AND endpoint = ? AND sandbox_id = ?", ownerID, projectID, endpoint, sandboxID).Error
}
