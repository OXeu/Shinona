package sqlite

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/settings"
)

type settingValue struct {
	Key   settings.Key
	Value string
}

type modelSettingValue struct {
	OwnerID string
	Key     settings.Key
	Value   string
}

func (r *Repository) SettingOverrides(ctx context.Context) (settings.OverrideSnapshot, error) {
	var rows []struct {
		Revision int64
		Key      *settings.Key
		Value    string
	}
	err := r.read.WithContext(ctx).Raw(`
		SELECT revision, key, COALESCE(value, '') AS value
		FROM instance_settings_revision
		LEFT JOIN (
			SELECT key, value FROM instance_setting_values
			UNION ALL
			SELECT key, value FROM instance_setting_secrets
		) ON TRUE
		WHERE id = 1
	`).Scan(&rows).Error
	if err != nil {
		return settings.OverrideSnapshot{}, err
	}
	if len(rows) == 0 {
		return settings.OverrideSnapshot{}, errors.New("missing instance settings revision")
	}
	result := settings.OverrideSnapshot{Revision: rows[0].Revision, Values: make(settings.Values, len(rows))}
	for _, row := range rows {
		if row.Key != nil {
			result.Values[*row.Key] = row.Value
		}
	}
	return result, nil
}

func (r *Repository) SaveSettingOverrides(ctx context.Context, expectedRevision int64, values settings.Values) (bool, error) {
	var updated bool
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Exec(`UPDATE instance_settings_revision SET revision = revision + 1
			WHERE id = 1 AND revision = ?`, expectedRevision)
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		if err := tx.Exec(`DELETE FROM instance_setting_values; DELETE FROM instance_setting_secrets`).Error; err != nil {
			return err
		}
		if err := insertInstanceSettings(tx, values); err != nil {
			return err
		}
		updated = true
		return nil
	})
	return err == nil && updated, err
}

func (r *Repository) ModelSettings(ctx context.Context, owner string) (settings.Values, error) {
	return readSettingValues(r.read.WithContext(ctx).Raw(`
		SELECT key, value FROM model_setting_values WHERE owner_id = ?
		UNION ALL
		SELECT key, value FROM model_setting_secrets WHERE owner_id = ?
	`, owner, owner))
}

func (r *Repository) SaveModelSettings(ctx context.Context, owner string, values settings.Values) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(values) == 0 {
			if err := tx.Select("id").Take(&userRow{}, "id = ?", owner).Error; err != nil {
				return err
			}
		}
		if err := tx.Exec("DELETE FROM model_setting_values WHERE owner_id = ?", owner).Error; err != nil {
			return err
		}
		if err := tx.Exec("DELETE FROM model_setting_secrets WHERE owner_id = ?", owner).Error; err != nil {
			return err
		}
		return insertModelSettings(tx, owner, values)
	})
}

func readSettingValues(query *gorm.DB) (settings.Values, error) {
	var rows []settingValue
	if err := query.Scan(&rows).Error; err != nil {
		return nil, err
	}
	values := make(settings.Values, len(rows))
	for _, row := range rows {
		values[row.Key] = row.Value
	}
	return values, nil
}

func insertInstanceSettings(tx *gorm.DB, values settings.Values) error {
	var public, secrets []settingValue
	for key, value := range values {
		entry := settingValue{Key: key, Value: value}
		if key.IsPublic() {
			public = append(public, entry)
		} else {
			secrets = append(secrets, entry)
		}
	}
	if len(public) != 0 {
		if err := tx.Table("instance_setting_values").CreateInBatches(public, 100).Error; err != nil {
			return err
		}
	}
	if len(secrets) != 0 {
		return tx.Table("instance_setting_secrets").CreateInBatches(secrets, 100).Error
	}
	return nil
}

func insertModelSettings(tx *gorm.DB, owner string, values settings.Values) error {
	var public, secrets []modelSettingValue
	for key, value := range values {
		entry := modelSettingValue{OwnerID: owner, Key: key, Value: value}
		if key.IsPublic() {
			public = append(public, entry)
		} else {
			secrets = append(secrets, entry)
		}
	}
	if len(public) != 0 {
		if err := tx.Table("model_setting_values").CreateInBatches(public, 100).Error; err != nil {
			return err
		}
	}
	if len(secrets) != 0 {
		return tx.Table("model_setting_secrets").CreateInBatches(secrets, 100).Error
	}
	return nil
}

var _ settings.Repository = (*Repository)(nil)
var _ settings.ModelRepository = (*Repository)(nil)
