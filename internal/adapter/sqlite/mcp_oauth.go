package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type mcpGrantRow struct {
	OwnerID string `gorm:"primaryKey"`
	Server  string `gorm:"primaryKey"`
	Grant   []byte `gorm:"not null"`
}

func (mcpGrantRow) TableName() string { return "mcp_oauth_grants" }

func (r *Repository) MCPGrant(ctx context.Context, owner, server string) ([]byte, error) {
	var record mcpGrantRow
	err := r.read.WithContext(ctx).Take(&record, "owner_id = ? AND server = ?", owner, server).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return record.Grant, err
}

func (r *Repository) SaveMCPGrant(ctx context.Context, owner, server string, data []byte) error {
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_id"}, {Name: "server"}}, DoUpdates: clause.AssignmentColumns([]string{"grant"}),
	}).Create(&mcpGrantRow{OwnerID: owner, Server: server, Grant: data}).Error
}

func (r *Repository) ReplaceMCPGrant(ctx context.Context, owner, server string, previous, data []byte) (bool, error) {
	result := r.db.WithContext(ctx).Model(&mcpGrantRow{}).
		Where("owner_id = ? AND server = ? AND grant = ?", owner, server, previous).
		UpdateColumn("grant", data)
	return result.RowsAffected == 1, result.Error
}

func (r *Repository) DeleteMCPGrant(ctx context.Context, owner, server string) error {
	return r.db.WithContext(ctx).Where("owner_id = ? AND server = ?", owner, server).Delete(&mcpGrantRow{}).Error
}

func (r *Repository) DeleteMCPGrantIfUnchanged(ctx context.Context, owner, server string, previous []byte) (bool, error) {
	result := r.db.WithContext(ctx).
		Where("owner_id = ? AND server = ? AND grant = ?", owner, server, previous).
		Delete(&mcpGrantRow{})
	return result.RowsAffected == 1, result.Error
}
