package sshsandbox

import (
	"crypto/sha256"
	"errors"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	projectIdleTimeout        = 30 * time.Second
	projectConnectionAge      = 5 * time.Minute
	maxIdleProjectConnections = 64
)

// ProjectConnections holds idle SSH transports for retained workspace requests.
// Each transport belongs to one lease at a time. The zero value is ready to use.
type ProjectConnections struct {
	mu     sync.Mutex
	idle   map[projectConnectionKey]*idleProjectConnection
	closed bool
}

type projectConnectionKey struct {
	owner    string
	identity [sha256.Size]byte
}

type projectConnection struct {
	client *ssh.Client
	opened time.Time
}

type idleProjectConnection struct {
	conn  *projectConnection
	timer *time.Timer
}

func (p *ProjectConnections) take(key projectConnectionKey) *projectConnection {
	p.mu.Lock()
	entry := p.idle[key]
	delete(p.idle, key)
	p.mu.Unlock()
	if entry == nil {
		return nil
	}
	if !entry.timer.Stop() {
		_ = entry.conn.client.Close()
		return nil
	}
	return entry.conn
}

func (p *ProjectConnections) put(key projectConnectionKey, conn *projectConnection) error {
	lifetime := min(projectIdleTimeout, projectConnectionAge-time.Since(conn.opened))
	p.mu.Lock()
	if p.closed || lifetime <= 0 || len(p.idle) >= maxIdleProjectConnections || p.idle[key] != nil {
		p.mu.Unlock()
		return conn.client.Close()
	}
	if p.idle == nil {
		p.idle = make(map[projectConnectionKey]*idleProjectConnection)
	}
	entry := &idleProjectConnection{conn: conn}
	entry.timer = time.AfterFunc(lifetime, func() {
		p.mu.Lock()
		if p.idle[key] != entry {
			p.mu.Unlock()
			return
		}
		delete(p.idle, key)
		p.mu.Unlock()
		_ = conn.client.Close()
	})
	p.idle[key] = entry
	p.mu.Unlock()
	return nil
}

// Close releases idle transports and closes outstanding transports when their
// leases are returned.
func (p *ProjectConnections) Close() error {
	p.mu.Lock()
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()
	var err error
	for _, entry := range idle {
		entry.timer.Stop()
		err = errors.Join(err, entry.conn.client.Close())
	}
	return err
}
