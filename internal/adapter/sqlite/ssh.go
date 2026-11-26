package sqlite

import (
	"context"
	"errors"

	"github.com/mattn/go-sqlite3"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/OXeu/shinona/internal/settings"
)

type sshServerRow struct {
	OwnerID         string `gorm:"primaryKey;uniqueIndex:idx_ssh_servers_owner_name"`
	ID              string `gorm:"primaryKey"`
	Name            string `gorm:"not null;uniqueIndex:idx_ssh_servers_owner_name"`
	Address         string
	User            string
	HostFingerprint string
	Template        string
	PrivateKey      string
}

func (sshServerRow) TableName() string { return "ssh_servers" }

const sshServerOrder = "name COLLATE NOCASE, id"

func (r *SettingsStore) SSHServers(ctx context.Context, owner string) ([]settings.SSHServerInfo, error) {
	servers := make([]settings.SSHServerInfo, 0)
	err := r.read.WithContext(ctx).Model(&sshServerRow{}).Select("id", "name", "address", "user", "host_fingerprint", "template").
		Where("owner_id = ?", owner).Order(sshServerOrder).Find(&servers).Error
	if err != nil {
		return nil, err
	}
	return servers, nil
}

func (r *SettingsStore) DefaultSSHServer(ctx context.Context, owner string) (settings.SSHServer, error) {
	record, err := readSSHServer(r.read.WithContext(ctx).Where("owner_id = ?", owner).Order(sshServerOrder))
	return record.server(), err
}

func (r *SettingsStore) SSHServer(ctx context.Context, owner, id string) (settings.SSHServer, error) {
	record, err := findSSHServer(r.read.WithContext(ctx), owner, id)
	return record.server(), err
}

func (r *SettingsStore) SaveSSHServer(ctx context.Context, owner string, server settings.SSHServer, create bool) error {
	db := r.db.WithContext(ctx)
	if create {
		result := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "owner_id"}, {Name: "name"}}, DoNothing: true}).Create(&sshServerRow{
			OwnerID: owner, ID: server.ID, Name: server.Name, Address: server.Address, User: server.User,
			HostFingerprint: server.HostFingerprint, Template: server.Template, PrivateKey: server.PrivateKey,
		})
		if result.Error == nil && result.RowsAffected == 0 {
			return settings.ErrSSHServerName
		}
		return result.Error
	}
	return db.Transaction(func(tx *gorm.DB) error {
		current, err := findSSHServer(tx, owner, server.ID)
		if err != nil {
			return err
		}
		var duplicates int64
		if err := tx.Model(&sshServerRow{}).Where("owner_id = ? AND name = ? AND id <> ?", owner, server.Name, server.ID).Count(&duplicates).Error; err != nil {
			return err
		}
		if duplicates != 0 {
			return settings.ErrSSHServerName
		}
		columns := map[string]any{
			"name": server.Name, "address": server.Address, "user": server.User,
			"host_fingerprint": server.HostFingerprint, "template": server.Template,
		}
		if server.PrivateKey != "" {
			columns["private_key"] = server.PrivateKey
		}
		return tx.Model(&current).UpdateColumns(columns).Error
	})
}

func (r *SettingsStore) RemoveSSHServer(ctx context.Context, owner, id string) error {
	result := r.db.WithContext(ctx).Delete(&sshServerRow{}, "owner_id = ? AND id = ?", owner, id)
	if err, ok := errors.AsType[sqlite3.Error](result.Error); ok && err.ExtendedCode == sqlite3.ErrConstraintForeignKey {
		return settings.ErrSSHServerInUse
	}
	if result.Error == nil && result.RowsAffected == 0 {
		return settings.ErrSSHServer
	}
	return result.Error
}

func findSSHServer(db *gorm.DB, owner, id string) (sshServerRow, error) {
	return readSSHServer(db.Where("owner_id = ? AND id = ?", owner, id))
}

func readSSHServer(db *gorm.DB) (sshServerRow, error) {
	var record sshServerRow
	err := db.Take(&record).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = settings.ErrSSHServer
	}
	return record, err
}

func (r sshServerRow) server() settings.SSHServer {
	return settings.SSHServer{
		SSHServerInfo: settings.SSHServerInfo{
			ID: r.ID, Name: r.Name, Address: r.Address, User: r.User,
			HostFingerprint: r.HostFingerprint, Template: r.Template,
		},
		PrivateKey: r.PrivateKey,
	}
}
