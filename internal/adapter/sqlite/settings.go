package sqlite

import (
	"context"
	"encoding/json"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/settings"
)

type instanceSettingsRow struct {
	ID   uint   `gorm:"primaryKey;autoIncrement:false"`
	Data []byte `gorm:"not null"`
}

const instanceSettingsID = 1

func (*instanceSettingsRow) TableName() string { return "instance_settings" }

type modelSettingsRow struct {
	OwnerID string `gorm:"primaryKey"`
	Data    []byte `gorm:"not null"`
}

func (*modelSettingsRow) TableName() string { return "model_settings" }

func (r *Repository) ModelSettings(ctx context.Context, owner string) (settings.Values, error) {
	values := make(settings.Values)
	var record modelSettingsRow
	err := r.read.WithContext(ctx).First(&record, "owner_id = ?", owner).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return values, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(record.Data, &values); err != nil {
		return nil, err
	}
	return values, nil
}

func (r *Repository) SaveModelSettings(ctx context.Context, owner string, values settings.Values) error {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	record := modelSettingsRow{OwnerID: owner, Data: data}
	return r.db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "owner_id"}}, DoUpdates: clause.AssignmentColumns([]string{"data"}),
	}).Create(&record).Error
}

func (r *Repository) SettingOverrides(ctx context.Context) (settings.Values, error) {
	result := make(settings.Values)
	var record instanceSettingsRow
	err := r.read.WithContext(ctx).First(&record, instanceSettingsID).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return result, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(record.Data, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (r *Repository) SaveSettingOverrides(ctx context.Context, values settings.Values) error {
	return saveSettingOverrides(r.db.WithContext(ctx), values)
}

func saveSettingOverrides(db *gorm.DB, values settings.Values) error {
	data, err := json.Marshal(values)
	if err != nil {
		return err
	}
	record := instanceSettingsRow{ID: instanceSettingsID, Data: data}
	return db.Clauses(clause.OnConflict{
		Columns: []clause.Column{{Name: "id"}}, DoUpdates: clause.AssignmentColumns([]string{"data"}),
	}).Create(&record).Error
}

var _ settings.Repository = (*Repository)(nil)
var _ settings.ModelRepository = (*Repository)(nil)
