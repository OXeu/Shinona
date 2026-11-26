package sqlite

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/settings"
)

type legacyInstanceSettingsRow struct {
	ID   uint   `gorm:"primaryKey;autoIncrement:false"`
	Data []byte `gorm:"not null"`
}

func (legacyInstanceSettingsRow) TableName() string { return "instance_settings" }

type legacyModelSettingsRow struct {
	OwnerID string `gorm:"primaryKey"`
	Data    []byte `gorm:"not null"`
}

func (legacyModelSettingsRow) TableName() string { return "model_settings" }

type legacyMCPSettingsRow struct {
	OwnerID string `gorm:"primaryKey"`
	Data    []byte `gorm:"not null"`
}

func (legacyMCPSettingsRow) TableName() string { return "mcp_settings" }

func migrateSettingsStorage(db *gorm.DB) error {
	const settingColumns = "key TEXT NOT NULL, value TEXT NOT NULL"
	if err := db.Exec(`
		CREATE TABLE instance_setting_values (` + settingColumns + `, PRIMARY KEY (key)) WITHOUT ROWID;
		CREATE TABLE instance_setting_secrets (` + settingColumns + `, PRIMARY KEY (key)) WITHOUT ROWID;
		CREATE TABLE model_setting_values (` + accountOwner + `, ` + settingColumns + `, PRIMARY KEY (owner_id, key)) WITHOUT ROWID;
		CREATE TABLE model_setting_secrets (` + accountOwner + `, ` + settingColumns + `, PRIMARY KEY (owner_id, key)) WITHOUT ROWID;
		CREATE TABLE mcp_servers (
			` + accountOwner + `, name TEXT NOT NULL, url TEXT NOT NULL,
			position INTEGER NOT NULL CHECK (position >= 0), oauth_client_id TEXT,
			PRIMARY KEY (owner_id, name), UNIQUE (owner_id, position)
		) WITHOUT ROWID;
		CREATE TABLE mcp_server_secrets (
			owner_id TEXT NOT NULL, name TEXT NOT NULL, bearer_token TEXT NOT NULL, client_secret TEXT NOT NULL,
			PRIMARY KEY (owner_id, name),
			FOREIGN KEY (owner_id, name) REFERENCES mcp_servers (owner_id, name) ON DELETE CASCADE,
			CHECK (bearer_token = '' OR client_secret = '')
		) WITHOUT ROWID;
	`).Error; err != nil {
		return err
	}
	if err := migrateInstanceSettings(db); err != nil {
		return err
	}
	if err := migrateModelSettings(db); err != nil {
		return err
	}
	if err := migrateMCPSettings(db); err != nil {
		return err
	}
	return db.Exec(`DROP TABLE instance_settings; DROP TABLE model_settings; DROP TABLE mcp_settings`).Error
}

func migrateInstanceSettings(db *gorm.DB) error {
	var record legacyInstanceSettingsRow
	err := db.Take(&record, "id = ?", 1).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	var values settings.Values
	if err := json.Unmarshal(record.Data, &values); err != nil {
		return fmt.Errorf("decode legacy instance settings: %w", err)
	}
	return insertInstanceSettings(db, values)
}

func migrateModelSettings(db *gorm.DB) error {
	var rows []legacyModelSettingsRow
	return db.FindInBatches(&rows, 100, func(batch *gorm.DB, _ int) error {
		for _, row := range rows {
			var values settings.Values
			if err := json.Unmarshal(row.Data, &values); err != nil {
				return fmt.Errorf("decode legacy model settings for account %q: %w", row.OwnerID, err)
			}
			if err := insertModelSettings(batch, row.OwnerID, values); err != nil {
				return err
			}
		}
		return nil
	}).Error
}

func migrateMCPSettings(db *gorm.DB) error {
	var rows []legacyMCPSettingsRow
	return db.FindInBatches(&rows, 100, func(batch *gorm.DB, _ int) error {
		for _, row := range rows {
			servers, err := decodeLegacyMCPSettings(row.Data)
			if err != nil {
				return fmt.Errorf("decode legacy MCP settings for account %q: %w", row.OwnerID, err)
			}
			if err := insertMCPConfig(batch, row.OwnerID, servers); err != nil {
				return err
			}
		}
		return nil
	}).Error
}

func decodeLegacyMCPSettings(data []byte) (settings.MCPConfig, error) {
	var entries settings.MCPConfig
	if len(bytes.TrimSpace(data)) != 0 {
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&entries); err != nil || !json.Valid(data) {
			return nil, errors.New("expected an array of MCP servers")
		}
	}
	for i := range entries {
		entries[i].Name = strings.TrimSpace(entries[i].Name)
	}
	return entries, entries.Validate()
}
