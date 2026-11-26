package sqlite

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/settings"
)

type mcpSettingsRow struct {
	OwnerID string `gorm:"primaryKey"`
	Data    []byte `gorm:"not null"`
}

func (mcpSettingsRow) TableName() string { return "mcp_settings" }

func (r *Repository) MCPConfig(ctx context.Context, owner string) (settings.MCPConfig, error) {
	var record mcpSettingsRow
	err := r.read.WithContext(ctx).Take(&record, "owner_id = ?", owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return settings.MCPConfig{}, nil
	}
	if err != nil {
		return nil, err
	}
	return settings.ParseMCP(string(record.Data))
}

func (r *Repository) SaveMCPConfig(ctx context.Context, owner string, config settings.MCPConfig, invalidated []string) error {
	data, err := json.Marshal(config)
	if err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record := mcpSettingsRow{OwnerID: owner, Data: data}
		if err := tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "owner_id"}}, DoUpdates: clause.AssignmentColumns([]string{"data"}),
		}).Create(&record).Error; err != nil {
			return err
		}
		if len(invalidated) == 0 {
			return nil
		}
		return tx.Where("owner_id = ? AND server IN ?", owner, invalidated).Delete(&mcpGrantRow{}).Error
	})
}

var _ settings.MCPRepository = (*Repository)(nil)

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
