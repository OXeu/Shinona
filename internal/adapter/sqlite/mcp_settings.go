package sqlite

import (
	"context"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/settings"
)

type mcpServerRow struct {
	OwnerID  string
	Name     string
	URL      string
	Position int
	// An empty client ID uses OAuth discovery. A nil value disables OAuth.
	OAuthClientID *string `gorm:"column:oauth_client_id"`
}

func (mcpServerRow) TableName() string { return "mcp_servers" }

type mcpServerSecretRow struct {
	OwnerID      string
	Name         string
	BearerToken  string
	ClientSecret string
}

func (mcpServerSecretRow) TableName() string { return "mcp_server_secrets" }

func (r *SettingsStore) MCPConfig(ctx context.Context, owner string) (settings.MCPConfig, error) {
	var rows []struct {
		Name          string
		URL           string
		OAuthClientID *string `gorm:"column:oauth_client_id"`
		BearerToken   string
		ClientSecret  string
	}
	err := r.read.WithContext(ctx).Raw(`
		SELECT s.name, s.url, s.oauth_client_id,
			COALESCE(c.bearer_token, '') AS bearer_token, COALESCE(c.client_secret, '') AS client_secret
		FROM mcp_servers s
		LEFT JOIN mcp_server_secrets c ON c.owner_id = s.owner_id AND c.name = s.name
		WHERE s.owner_id = ? ORDER BY s.position
	`, owner).Scan(&rows).Error
	if err != nil {
		return nil, err
	}
	servers := make(settings.MCPConfig, 0, len(rows))
	for _, row := range rows {
		server := settings.MCPServer{Name: row.Name, URL: row.URL, Token: row.BearerToken}
		if row.OAuthClientID != nil {
			server.OAuth = &settings.MCPOAuthConfig{ClientID: *row.OAuthClientID, ClientSecret: row.ClientSecret}
		}
		servers = append(servers, server)
	}
	return servers, servers.Validate()
}

func (r *SettingsStore) SaveMCPConfig(ctx context.Context, owner string, config settings.MCPConfig, invalidated []string) error {
	if err := config.Validate(); err != nil {
		return err
	}
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if len(config) == 0 {
			if err := tx.Select("id").Take(&userRow{}, "id = ?", owner).Error; err != nil {
				return err
			}
		}
		if err := tx.Delete(&mcpServerRow{}, "owner_id = ?", owner).Error; err != nil {
			return err
		}
		if err := insertMCPConfig(tx, owner, config); err != nil {
			return err
		}
		if len(invalidated) == 0 {
			return nil
		}
		return tx.Where("owner_id = ? AND server IN ?", owner, invalidated).Delete(&mcpGrantRow{}).Error
	})
}

func insertMCPConfig(tx *gorm.DB, owner string, config settings.MCPConfig) error {
	if len(config) == 0 {
		return nil
	}
	servers := make([]mcpServerRow, 0, len(config))
	var secrets []mcpServerSecretRow
	for i, server := range config {
		row := mcpServerRow{OwnerID: owner, Name: server.Name, URL: server.URL, Position: i}
		credentials := mcpServerSecretRow{OwnerID: owner, Name: server.Name, BearerToken: server.Token}
		if server.OAuth != nil {
			row.OAuthClientID = &server.OAuth.ClientID
			credentials.ClientSecret = server.OAuth.ClientSecret
		}
		servers = append(servers, row)
		if credentials.BearerToken != "" || credentials.ClientSecret != "" {
			secrets = append(secrets, credentials)
		}
	}
	if err := tx.Create(&servers).Error; err != nil {
		return err
	}
	if len(secrets) != 0 {
		return tx.Create(&secrets).Error
	}
	return nil
}

var _ settings.MCPRepository = (*SettingsStore)(nil)
