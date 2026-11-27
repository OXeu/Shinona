package sshsandbox

import (
	"errors"
	"slices"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	projectIdleTimeout        = 30 * time.Second
	projectConnectionAge      = 5 * time.Minute
	maxIdleProjectConnections = 64
	maxIdleConnectionsPerKey  = 4
)

// ProjectConnections holds idle SSH transports for retained workspace requests.
// Each transport belongs to one lease at a time. The zero value is ready to use.
type ProjectConnections struct {
	mu     sync.Mutex
	idle   map[projectConnectionKey][]*idleProjectConnection
	count  int
	closed bool
}

type projectConnectionKey struct {
	owner    string
	identity connectionIdentity
}

type connectionIdentity struct {
	address, user, hostFingerprint, keyFingerprint string
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
	for {
		p.mu.Lock()
		entries := p.idle[key]
		if len(entries) == 0 {
			p.mu.Unlock()
			return nil
		}
		last := len(entries) - 1
		entry := entries[last]
		p.removeIdle(key, entries, last)
		p.mu.Unlock()
		if entry.timer.Stop() {
			return entry.conn
		}
		_ = entry.conn.client.Close()
	}
}

func (p *ProjectConnections) removeIdle(key projectConnectionKey, entries []*idleProjectConnection, index int) {
	if len(entries) == 1 {
		delete(p.idle, key)
	} else {
		p.idle[key] = slices.Delete(entries, index, index+1)
	}
	p.count--
}

func (p *ProjectConnections) put(key projectConnectionKey, conn *projectConnection) error {
	lifetime := min(projectIdleTimeout, projectConnectionAge-time.Since(conn.opened))
	p.mu.Lock()
	if p.closed || lifetime <= 0 || p.count >= maxIdleProjectConnections || len(p.idle[key]) >= maxIdleConnectionsPerKey {
		p.mu.Unlock()
		return conn.client.Close()
	}
	if p.idle == nil {
		p.idle = make(map[projectConnectionKey][]*idleProjectConnection)
	}
	entry := &idleProjectConnection{conn: conn}
	entry.timer = time.AfterFunc(lifetime, func() {
		p.mu.Lock()
		entries := p.idle[key]
		index := slices.Index(entries, entry)
		if index < 0 {
			p.mu.Unlock()
			return
		}
		p.removeIdle(key, entries, index)
		p.mu.Unlock()
		_ = conn.client.Close()
	})
	p.idle[key] = append(p.idle[key], entry)
	p.count++
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
	p.count = 0
	p.mu.Unlock()
	var err error
	for _, entries := range idle {
		for _, entry := range entries {
			entry.timer.Stop()
			err = errors.Join(err, entry.conn.client.Close())
		}
	}
	return err
}
