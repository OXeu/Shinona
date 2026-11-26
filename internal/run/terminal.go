package run

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
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
	project string
	ctx     context.Context
	cancel  context.CancelFunc
	done    chan struct{}
	mu      sync.Mutex
	shell   sandbox.Terminal
	state   TerminalState
	history []byte
	end     uint64
	changed chan struct{}
}

type terminalRegistry struct {
	mu      sync.Mutex
	tabs    map[sessionKey][]*TerminalSession
	running map[*TerminalSession]sessionKey
	starts  map[sessionKey]*terminalStarts
	closed  bool
	drained chan struct{}
}

type terminalStarts struct{ count int }

func (s *Service) Terminals(ctx context.Context, owner, id string) ([]TerminalTab, error) {
	if _, err := s.stores.Sessions.SessionProject(ctx, owner, id); err != nil {
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
	key := sessionKey{owner, id}
	start := pool.beginStart(key)
	if start == nil {
		return TerminalTab{}, ErrTerminalUnavailable
	}
	defer pool.endStart(key, start)
	spec, release, err := lockSessionProject(ctx, s.stores.Sessions, owner, id, func(key sandbox.ProjectKey) (func(), error) {
		if release, ok := s.projectChanges.TryAcquire(key, true); ok {
			return release, nil
		}
		return nil, ErrWorkspaceBusy
	})
	if err != nil {
		return TerminalTab{}, err
	}
	defer release()
	spec, target, err := projectTarget(ctx, s.stores.Projects, s.resolve, spec, "", true)
	if err != nil {
		return TerminalTab{}, err
	}
	provider, ok := target.Provider.(sandbox.ProjectProvider)
	if !ok {
		return TerminalTab{}, ErrTerminalUnavailable
	}
	pool.mu.Lock()
	defer pool.mu.Unlock()
	if pool.closed || ctx.Err() != nil || lifetime.Err() != nil || pool.starts[key] != start {
		return TerminalTab{}, ErrTerminalUnavailable
	}
	var accountCount, sessionCount int
	for _, conversation := range pool.running {
		if conversation.owner == owner {
			accountCount++
			if conversation == key {
				sessionCount++
			}
		}
	}
	if sessionCount >= terminalsPerSession || accountCount >= terminalsPerAccount {
		return TerminalTab{}, ErrTerminalLimit
	}
	number := 1
	tabs := pool.tabs[key]
	if len(tabs) != 0 {
		number = tabs[len(tabs)-1].tab.Number + 1
	}
	shellCtx, cancel := context.WithCancel(lifetime)
	entry := &TerminalSession{
		tab: TerminalTab{ID: s.newID(), Number: number}, ctx: shellCtx, cancel: cancel,
		project: spec.ProjectID, state: TerminalStarting, changed: make(chan struct{}), done: make(chan struct{}),
	}
	if pool.tabs == nil {
		pool.tabs = make(map[sessionKey][]*TerminalSession)
		pool.running = make(map[*TerminalSession]sessionKey)
	}
	pool.tabs[key] = append(tabs, entry)
	pool.running[entry] = key
	spec.RunID = entry.tab.ID
	go pool.runTerminal(key, entry, provider, spec)
	return entry.tab, nil
}

// An archive or account deletion invalidates any start being validated
// concurrently. Validation itself runs outside the registry mutex.
func (p *terminalRegistry) beginStart(key sessionKey) *terminalStarts {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	if p.starts == nil {
		p.starts = make(map[sessionKey]*terminalStarts)
	}
	start := p.starts[key]
	if start == nil {
		start = new(terminalStarts)
		p.starts[key] = start
	}
	start.count++
	return start
}

func (p *terminalRegistry) endStart(key sessionKey, start *terminalStarts) {
	p.mu.Lock()
	defer p.mu.Unlock()
	start.count--
	if start.count == 0 && p.starts[key] == start {
		delete(p.starts, key)
	}
}

func (p *terminalRegistry) runTerminal(key sessionKey, entry *TerminalSession, provider sandbox.ProjectProvider, spec sandbox.Spec) {
	defer func() {
		entry.cancel()
		p.mu.Lock()
		defer p.mu.Unlock()
		p.removeTab(key, entry)
		delete(p.running, entry)
		close(entry.done)
		if p.closed && len(p.running) == 0 {
			close(p.drained)
		}
	}()
	err := entry.run(provider, spec)
	entry.mu.Lock()
	entry.state = TerminalExited
	failed := err != nil && !errors.Is(err, io.EOF) && entry.ctx.Err() == nil
	if failed {
		entry.state = TerminalFailed
	}
	entry.shell = nil
	entry.signal()
	entry.mu.Unlock()
	if failed {
		slog.Warn("workspace terminal", "terminal_id", entry.tab.ID, "error", err)
	}
}

func (p *terminalRegistry) stopProject(ctx context.Context, owner, project string) error {
	p.mu.Lock()
	var closing []*TerminalSession
	for entry, key := range p.running {
		if key.owner == owner && entry.project == project {
			entry.cancel()
			closing = append(closing, entry)
		}
	}
	p.mu.Unlock()
	for _, entry := range closing {
		select {
		case <-entry.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func (s *Service) Terminal(ctx context.Context, owner, id, terminalID string) (*TerminalSession, error) {
	if _, err := s.stores.Sessions.SessionProject(ctx, owner, id); err != nil {
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
	if _, err := s.stores.Sessions.SessionProject(ctx, owner, id); err != nil {
		return err
	}
	s.terminals.mu.Lock()
	defer s.terminals.mu.Unlock()
	key := sessionKey{owner, id}
	tabs := s.terminals.tabs[key]
	for _, entry := range tabs {
		if entry.tab.ID != terminalID {
			continue
		}
		entry.cancel()
		s.terminals.removeTab(key, entry)
		return nil
	}
	return ErrTerminalNotFound
}

// removeTab requires p.mu. Existing viewers retain the terminal until their
// relay has delivered the last output.
func (p *terminalRegistry) removeTab(key sessionKey, target *TerminalSession) {
	tabs := p.tabs[key]
	for i, entry := range tabs {
		if entry != target {
			continue
		}
		copy(tabs[i:], tabs[i+1:])
		tabs[len(tabs)-1] = nil
		if len(tabs) == 1 {
			delete(p.tabs, key)
		} else {
			p.tabs[key] = tabs[:len(tabs)-1]
		}
		return
	}
}

func (p *terminalRegistry) closeSession(key sessionKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, entry := range p.tabs[key] {
		entry.cancel()
	}
	delete(p.tabs, key)
	delete(p.starts, key)
}

func (s *Service) CloseTerminals(ctx context.Context) error {
	p := &s.terminals
	p.mu.Lock()
	if !p.closed {
		p.closed = true
		p.drained = make(chan struct{})
		for entry := range p.running {
			entry.cancel()
		}
		clear(p.tabs)
		clear(p.starts)
		if len(p.running) == 0 {
			close(p.drained)
		}
	}
	drained := p.drained
	p.mu.Unlock()
	select {
	case <-drained:
		return nil
	default:
	}
	select {
	case <-drained:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Service) AccountDeleted(owner string) {
	s.controls.stopAccount(owner)
	p := &s.terminals
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, tabs := range p.tabs {
		if key.owner != owner {
			continue
		}
		for _, entry := range tabs {
			entry.cancel()
		}
		delete(p.tabs, key)
	}
	for key := range p.starts {
		if key.owner == owner {
			delete(p.starts, key)
		}
	}
}

func (t *TerminalSession) run(provider sandbox.ProjectProvider, spec sandbox.Spec) error {
	ctx, phases := timing.WithRecorder(t.ctx)
	acquire := timing.Start(ctx, "sandbox_acquire")
	workspace, err := provider.Acquire(ctx, spec)
	acquire()
	if err != nil {
		slog.Info("terminal startup timing", "phases", phases)
		return err
	}
	defer closeTerminalResource(t.ctx, workspace.Close)
	terminal, ok := workspace.(sandbox.TerminalWorkspace)
	if !ok {
		slog.Info("terminal startup timing", "phases", phases)
		return ErrTerminalUnavailable
	}
	open := timing.Start(ctx, "pty_open")
	shell, err := terminal.OpenTerminal(ctx, sandbox.TerminalSize{Cols: 80, Rows: 24})
	open()
	slog.Info("terminal startup timing", "phases", phases)
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

// ReplayEnd is the byte offset where output already produced at attach ends.
func (t *TerminalSession) ReplayEnd() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.end
}

// Output reads the retained byte stream by offset. A new viewer starts at
// offset zero and a slow viewer resumes at the oldest bytes still retained.
func (t *TerminalSession) Output(offset uint64, buffer []byte) (uint64, int, TerminalState, <-chan struct{}) {
	t.mu.Lock()
	defer t.mu.Unlock()
	offset = min(t.end, max(offset, t.end-min(t.end, terminalHistorySize)))
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
