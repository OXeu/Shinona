package sqlite

import (
	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/settings"
)

func migrateProjectSSHBindings(db *gorm.DB) error {
	var definitions []string
	if err := db.Raw(`SELECT sql FROM sqlite_schema
		WHERE tbl_name = 'project_sandbox_settings' AND sql IS NOT NULL
		AND type IN ('index', 'trigger') ORDER BY type, name`).Scan(&definitions).Error; err != nil {
		return err
	}
	if err := db.Exec(`CREATE TABLE new_project_sandbox_settings (
		` + accountOwner + `, project_id TEXT NOT NULL, provider TEXT, ssh_server_id TEXT,
		template TEXT, timeout_seconds INTEGER, on_timeout TEXT,
		` + projectRef + `, PRIMARY KEY (owner_id, project_id), ` + projectFK + ` ON DELETE CASCADE,
		FOREIGN KEY (owner_id, ssh_server_id) REFERENCES ssh_servers (owner_id, id),
		CHECK (ssh_server_id IS NULL OR provider IS '` + sandbox.ProviderSSH + `')
	)`).Error; err != nil {
		return err
	}
	// Keep unavailable legacy choices visible while creating a foreign key only
	// for servers that still belong to the project owner.
	if err := db.Exec(`INSERT INTO new_project_sandbox_settings
		(owner_id, project_id, provider, ssh_server_id, template, timeout_seconds, on_timeout)
		SELECT config.owner_id, config.project_id, CASE WHEN server.id IS NULL THEN config.provider ELSE ? END,
		server.id, config.template, config.timeout_seconds, config.on_timeout
		FROM project_sandbox_settings AS config
		LEFT JOIN ssh_servers AS server ON server.owner_id = config.owner_id AND config.provider = ? || server.id`,
		sandbox.ProviderSSH, settings.SSHProviderName("")).Error; err != nil {
		return err
	}
	if err := db.Exec(`DROP TABLE project_sandbox_settings;
		ALTER TABLE new_project_sandbox_settings RENAME TO project_sandbox_settings`).Error; err != nil {
		return err
	}
	for _, definition := range definitions {
		if err := db.Exec(definition).Error; err != nil {
			return err
		}
	}
	return db.Exec(`CREATE INDEX idx_project_sandbox_ssh_server ON project_sandbox_settings (owner_id, ssh_server_id)
		WHERE ssh_server_id IS NOT NULL`).Error
}
