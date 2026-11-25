package run

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

const (
	terminalsPerSession = 8
	terminalsPerAccount = 32
	terminalHistorySize = 256 << 10
)

var (
	ErrTerminalUnavailable = errors.New("terminal is unavailable for this workspace")
	ErrTerminalNotFound    = errors.New("terminal does not exist in this conversation")
	ErrTerminalLimit       = errors.New("terminal limit reached")
)

type TerminalTab struct {
	ID     string `json:"id"`
	Number int    `json:"number"`
}

type TerminalState string

const (
	TerminalStarting TerminalState = "starting"
	TerminalRunning  TerminalState = "running"
	TerminalExited   TerminalState = "exited"
	TerminalFailed   TerminalState = "failed"
)

// TerminalSession owns a shell independently of any browser connection.
type TerminalSession struct {
	tab     TerminalTab
	ctx     context.Context
	cancel  context.CancelFunc
	mu      sync.Mutex
	shell   sandbox.Terminal
	state   TerminalState
	history []byte
	end     uint64
	changed chan struct{}
}

type terminalRegistry struct {
	mu     sync.Mutex
	tabs   map[sessionKey][]*TerminalSession
	closed bool
	active sync.WaitGroup
}

func (s *Service) Terminals(ctx context.Context, owner, id string) ([]TerminalTab, error) {
	if _, err := s.repo.SessionProject(ctx, owner, id); err != nil {
		return nil, err
	}
	s.terminals.mu.Lock()
	defer s.terminals.mu.Unlock()
	tabs := s.terminals.tabs[sessionKey{owner, id}]
	result := make([]TerminalTab, len(tabs))
	for i, tab := range tabs {
		result[i] = tab.tab
	}
	return result, nil
}

func (s *Service) StartTerminal(ctx, lifetime context.Context, owner, id string) (TerminalTab, error) {
	pool := &s.terminals
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed || lifetime.Err() != nil {
		return TerminalTab{}, ErrTerminalUnavailable
	}
	// Archive commits before it removes the tabs under this same mutex.
	projectID, err := s.repo.SessionProject(ctx, owner, id)
	if err != nil {
		return TerminalTab{}, err
	}
	provider, ok := s.resolve("").Provider.(sandbox.ProjectProvider)
	if !ok {
		return TerminalTab{}, ErrTerminalUnavailable
	}
	key := sessionKey{owner, id}
	tabs := pool.tabs[key]
	count := 0
	for conversation, entries := range pool.tabs {
		if conversation.owner == owner {
			count += len(entries)
		}
	}
	if len(tabs) >= terminalsPerSession || count >= terminalsPerAccount {
		return TerminalTab{}, ErrTerminalLimit
	}
	number := 1
	if len(tabs) > 0 {
		number = tabs[len(tabs)-1].tab.Number + 1
	}
	shellCtx, cancel := context.WithCancel(lifetime)
	entry := &TerminalSession{
		tab: TerminalTab{ID: s.newID(), Number: number}, ctx: shellCtx, cancel: cancel,
		state: TerminalStarting, changed: make(chan struct{}),
	}
	if pool.tabs == nil {
		pool.tabs = make(map[sessionKey][]*TerminalSession)
	}
	pool.tabs[key] = append(tabs, entry)
	pool.active.Add(1)
	go func() {
		defer pool.active.Done()
		defer cancel()
		err := entry.run(provider, sandbox.Spec{
			RunID: entry.tab.ID, OwnerID: owner, SessionID: id, ProjectID: projectID, Policy: sandbox.Shared,
		})
		entry.mu.Lock()
		defer entry.mu.Unlock()
		entry.state = TerminalExited
		if err != nil && !errors.Is(err, io.EOF) && shellCtx.Err() == nil {
			entry.state = TerminalFailed
			slog.Warn("workspace terminal", "terminal_id", entry.tab.ID, "error", err)
		}
		entry.shell = nil
		entry.signal()
	}()
	return entry.tab, nil
}

func (s *Service) Terminal(ctx context.Context, owner, id, terminalID string) (*TerminalSession, error) {
	if _, err := s.repo.SessionProject(ctx, owner, id); err != nil {
		return nil, err
	}
	s.terminals.mu.Lock()
	defer s.terminals.mu.Unlock()
	for _, entry := range s.terminals.tabs[sessionKey{owner, id}] {
		if entry.tab.ID == terminalID {
			return entry, nil
		}
	}
	return nil, ErrTerminalNotFound
}

func (s *Service) StopTerminal(ctx context.Context, owner, id, terminalID string) error {
	if _, err := s.repo.SessionProject(ctx, owner, id); err != nil {
		return err
	}
	s.terminals.mu.Lock()
	defer s.terminals.mu.Unlock()
	key := sessionKey{owner, id}
	tabs := s.terminals.tabs[key]
	for i, entry := range tabs {
		if entry.tab.ID != terminalID {
			continue
		}
		entry.cancel()
		copy(tabs[i:], tabs[i+1:])
		tabs[len(tabs)-1] = nil
		if len(tabs) == 1 {
			delete(s.terminals.tabs, key)
		} else {
			s.terminals.tabs[key] = tabs[:len(tabs)-1]
		}
		return nil
	}
	return ErrTerminalNotFound
}

func (p *terminalRegistry) closeSession(key sessionKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.tabs[key] {
		entry.cancel()
	}
	delete(p.tabs, key)
}

func (s *Service) CloseTerminals() {
	p := &s.terminals
	p.mu.Lock()
	p.closed = true
	for _, tabs := range p.tabs {
		for _, entry := range tabs {
			entry.cancel()
		}
	}
	clear(p.tabs)
	p.mu.Unlock()
	p.active.Wait()
}

func (t *TerminalSession) run(provider sandbox.ProjectProvider, spec sandbox.Spec) error {
	workspace, err := provider.Acquire(t.ctx, spec)
	if err != nil {
		return err
	}
	defer closeTerminalResource(t.ctx, workspace.Close)
	terminal, ok := workspace.(sandbox.TerminalWorkspace)
	if !ok {
		return ErrTerminalUnavailable
	}
	shell, err := terminal.OpenTerminal(t.ctx, sandbox.TerminalSize{Cols: 80, Rows: 24})
	if err != nil {
		return err
	}
	defer closeTerminalResource(t.ctx, shell.Close)
	t.mu.Lock()
	t.shell = shell
	t.state = TerminalRunning
	t.signal()
	t.mu.Unlock()

	var buffer [sandbox.TerminalChunkSize]byte
	for {
		n, err := shell.Read(buffer[:])
		if n > 0 {
			t.appendOutput(buffer[:n])
		}
		if err != nil {
			return err
		}
	}
}

func closeTerminalResource(ctx context.Context, close func(context.Context) error) {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	_ = close(cleanup)
}

func (t *TerminalSession) signal() {
	close(t.changed)
	t.changed = make(chan struct{})
}

func (t *TerminalSession) appendOutput(data []byte) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.history == nil {
		t.history = make([]byte, terminalHistorySize)
	}
	start := int(t.end % terminalHistorySize)
	n := copy(t.history[start:], data)
	copy(t.history, data[n:])
	t.end += uint64(len(data))
	t.signal()
}

// Output reads the retained byte stream by offset. A new viewer starts at
// offset zero and a slow viewer resumes at the oldest bytes still retained.
func (t *TerminalSession) Output(offset uint64, buffer []byte) (uint64, int, TerminalState, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	offset = max(offset, t.end-min(t.end, terminalHistorySize))
	n := min(int(t.end-offset), len(buffer))
	if n > 0 {
		start := int(offset % terminalHistorySize)
		first := copy(buffer[:n], t.history[start:])
		copy(buffer[first:n], t.history)
	}
	return offset + uint64(n), n, t.state, t.changed
}

func (t *TerminalSession) Input(ctx context.Context, data []byte) error {
	shell := t.runningShell()
	if shell == nil {
		return ErrTerminalUnavailable
	}
	return shell.Input(ctx, data)
}

func (t *TerminalSession) Resize(ctx context.Context, size sandbox.TerminalSize) error {
	shell := t.runningShell()
	if shell == nil {
		return ErrTerminalUnavailable
	}
	return shell.Resize(ctx, size)
}

func (t *TerminalSession) runningShell() sandbox.Terminal {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state != TerminalRunning || t.ctx.Err() != nil {
		return nil
	}
	return t.shell
}
