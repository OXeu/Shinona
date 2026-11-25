package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/run"
)

type composerDefaultsRow struct {
	OwnerID    string `gorm:"primaryKey"`
	Model      string `gorm:"not null"`
	Permission string `gorm:"not null"`
	Thinking   string `gorm:"not null"`
}

func (composerDefaultsRow) TableName() string { return "composer_defaults" }

func (r *Repository) ComposerDefaults(ctx context.Context, ownerID string) (*run.ComposerDefaults, error) {
	var record composerDefaultsRow
	err := r.db.WithContext(ctx).Take(&record, "owner_id = ?", ownerID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &run.ComposerDefaults{Model: record.Model, Permission: record.Permission, Thinking: record.Thinking}, nil
}

func (r *Repository) SaveComposerDefaults(ctx context.Context, ownerID string, defaults run.ComposerDefaults) error {
	record := composerDefaultsRow{OwnerID: ownerID, Model: defaults.Model, Permission: defaults.Permission, Thinking: defaults.Thinking}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_id"}}, DoUpdates: clause.AssignmentColumns([]string{"model", "permission", "thinking"}),
	}).Create(&record).Error
}
