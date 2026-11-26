package sqlite

import (
	"context"
	"errors"

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

func (r *Repository) SSHServers(ctx context.Context, owner string) ([]settings.SSHServer, error) {
	var records []sshServerRow
	err := r.db.WithContext(ctx).Omit("private_key").Where("owner_id = ?", owner).Order("name COLLATE NOCASE, id").Find(&records).Error
	if err != nil {
		return nil, err
	}
	servers := make([]settings.SSHServer, len(records))
	for i, record := range records {
		servers[i] = record.server()
	}
	return servers, nil
}

func (r *Repository) SSHServer(ctx context.Context, owner, id string) (settings.SSHServer, error) {
	record, err := findSSHServer(r.db.WithContext(ctx), owner, id)
	return record.server(), err
}

func (r *Repository) SaveSSHServer(ctx context.Context, owner string, server settings.SSHServer, create bool) error {
	db := r.db.WithContext(ctx)
	if create {
		result := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&sshServerRow{
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
		return tx.Model(&current).UpdateColumns(map[string]any{
			"name": server.Name, "address": server.Address, "user": server.User,
			"host_fingerprint": server.HostFingerprint, "template": server.Template, "private_key": server.PrivateKey,
		}).Error
	})
}

func (r *Repository) RemoveSSHServer(ctx context.Context, owner, id string) error {
	return r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		record, err := findSSHServer(tx, owner, id)
		if err != nil {
			return err
		}
		if err := ensureSSHServerUnused(tx, owner, id); err != nil {
			return err
		}
		return tx.Delete(&record).Error
	})
}

func ensureSSHServerUnused(tx *gorm.DB, owner, id string) error {
	var count int64
	if err := tx.Model(&projectSandboxRow{}).Where("owner_id = ? AND provider = ?", owner, settings.SSHProviderPrefix+id).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return settings.ErrSSHServerInUse
	}
	return nil
}

func findSSHServer(db *gorm.DB, owner, id string) (sshServerRow, error) {
	var record sshServerRow
	err := db.Take(&record, "owner_id = ? AND id = ?", owner, id).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		err = settings.ErrSSHServer
	}
	return record, err
}

func (r sshServerRow) server() settings.SSHServer {
	return settings.SSHServer{
		ID: r.ID, Name: r.Name, Address: r.Address, User: r.User,
		HostFingerprint: r.HostFingerprint, Template: r.Template, PrivateKey: r.PrivateKey,
	}
}
