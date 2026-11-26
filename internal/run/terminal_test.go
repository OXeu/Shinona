package run

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestTerminalStartDoesNotBlockRegistryAndArchiveInvalidatesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const owner, session = "owner", "session"
		key := sessionKey{owner, session}
		repo := archivingTerminalRepository{workspaceTestRepository{projects: map[sessionKey]string{key: ""}}}
		resolve := make(chan struct{})
		service := NewService(ServiceStores{Sessions: repo, Projects: repo}, func(context.Context, string, string) (Target, error) {
			<-resolve
			return Target{Provider: workspaceTestProvider{}}, nil
		}, nil, &Controls{}, nil, DefaultWorkspaceTimeouts())
		started := make(chan error, 1)
		go func() {
			_, err := service.StartTerminal(t.Context(), t.Context(), owner, session)
			started <- err
		}()
		synctest.Wait()
		if tabs, err := service.Terminals(t.Context(), owner, session); err != nil || len(tabs) != 0 {
			t.Fatalf("could not list tabs during a pending start: %v, %v", tabs, err)
		}
		if err := service.ArchiveSession(t.Context(), owner, session); err != nil {
			t.Fatal(err)
		}
		close(resolve)
		if err := <-started; !errors.Is(err, ErrTerminalUnavailable) {
			t.Fatalf("start racing with archive = %v", err)
		}
	})
}

func TestTerminalExitReleasesQuotaAndShutdownHasDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const owner, session = "owner", "session"
		key := sessionKey{owner, session}
		repo := workspaceTestRepository{projects: map[sessionKey]string{key: ""}}
		acquire := make(chan struct{})
		nextID := 0
		service := NewService(ServiceStores{Sessions: repo, Projects: repo}, func(context.Context, string, string) (Target, error) {
			return Target{Provider: pendingTerminalProvider{acquire: acquire}}, nil
		}, func() string {
			nextID++
			return fmt.Sprint(nextID)
		}, &Controls{}, nil, DefaultWorkspaceTimeouts())
		start := func() (TerminalTab, error) {
			return service.StartTerminal(t.Context(), t.Context(), owner, session)
		}
		for range terminalsPerSession {
			if _, err := start(); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := start(); !errors.Is(err, ErrTerminalLimit) {
			t.Fatalf("start at capacity = %v", err)
		}
		acquire <- struct{}{}
		synctest.Wait()
		if tabs, err := service.Terminals(t.Context(), owner, session); err != nil || len(tabs) != terminalsPerSession-1 {
			t.Fatalf("exited terminal retained its tab: %v, %v", tabs, err)
		}
		if _, err := start(); err != nil {
			t.Fatalf("exited terminal retained its quota: %v", err)
		}
		shutdown, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := service.CloseTerminals(shutdown); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("shutdown waiting for a stuck provider = %v", err)
		}
		close(acquire)
		if err := service.CloseTerminals(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

type archivingTerminalRepository struct{ workspaceTestRepository }

func (archivingTerminalRepository) ArchiveSession(context.Context, string, string, time.Time) error {
	return nil
}

type pendingTerminalProvider struct {
	workspaceTestProvider
	acquire <-chan struct{}
}

func (p pendingTerminalProvider) Acquire(context.Context, sandbox.Spec) (sandbox.Workspace, error) {
	<-p.acquire
	return &stubWorkspace{}, nil
}

func TestTerminalStartupDeadlineCoversAcquireAndOpen(t *testing.T) {
	for _, stage := range []string{"acquire", "open", "running"} {
		t.Run(stage, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const owner, session = "owner", "session"
				limits := DefaultWorkspaceTimeouts()
				limits.Lifecycle = time.Second
				repo := workspaceTestRepository{projects: map[sessionKey]string{{owner, session}: ""}}
				box := &startupWorkspace{open: func(ctx context.Context) (sandbox.Terminal, error) {
					if stage == "open" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return startupTerminal{ctx: ctx}, nil
				}}
				provider := startupProvider{acquire: func(ctx context.Context) (sandbox.Workspace, error) {
					if stage == "acquire" {
						<-ctx.Done()
						return nil, ctx.Err()
					}
					return box, nil
				}}
				resolve := func(context.Context, string, string) (Target, error) { return Target{Provider: provider}, nil }
				service := NewService(ServiceStores{Sessions: repo, Projects: repo}, resolve, func() string { return "tab" }, &Controls{}, nil, limits)
				tab, err := service.StartTerminal(t.Context(), t.Context(), owner, session)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = service.CloseTerminals(t.Context()) }()
				entry, err := service.Terminal(t.Context(), owner, session, tab.ID)
				if err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				time.Sleep(limits.Lifecycle + time.Millisecond)
				synctest.Wait()
				tabs, err := service.Terminals(t.Context(), owner, session)
				if err != nil {
					t.Fatal(err)
				}
				_, _, state, _ := entry.Output(0, nil)
				if stage == "running" {
					if state != TerminalRunning || len(tabs) != 1 {
						t.Fatalf("startup timeout ended a running shell: %s, %v", state, tabs)
					}
				} else if state != TerminalFailed || len(tabs) != 0 {
					t.Fatalf("stalled %s did not fail and release its tab: %s, %v", stage, state, tabs)
				}
			})
		})
	}
}

type startupProvider struct {
	workspaceTestProvider
	acquire func(context.Context) (sandbox.Workspace, error)
}

func (p startupProvider) Acquire(ctx context.Context, _ sandbox.Spec) (sandbox.Workspace, error) {
	return p.acquire(ctx)
}

type startupWorkspace struct {
	stubWorkspace
	open func(context.Context) (sandbox.Terminal, error)
}

func (w startupWorkspace) OpenTerminal(ctx context.Context, _ sandbox.TerminalSize) (sandbox.Terminal, error) {
	return w.open(ctx)
}

type startupTerminal struct {
	sandbox.Terminal
	ctx context.Context
}

func (t startupTerminal) Read([]byte) (int, error) {
	<-t.ctx.Done()
	return 0, t.ctx.Err()
}

func (startupTerminal) Close(context.Context) error { return nil }

func TestTerminalOutputResumesAtRetainedHistory(t *testing.T) {
	tab := TerminalSession{state: TerminalRunning, changed: make(chan struct{})}
	chunk := bytes.Repeat([]byte{'x'}, sandbox.TerminalChunkSize)
	for range terminalHistorySize / len(chunk) {
		tab.appendOutput(chunk)
	}
	buffer := make([]byte, terminalHistorySize)
	offset, n, state, changed := tab.Output(0, buffer)
	if offset != terminalHistorySize || n != terminalHistorySize || state != TerminalRunning {
		t.Fatalf("initial replay: offset=%d bytes=%d state=%s", offset, n, state)
	}
	tab.appendOutput([]byte("new output"))
	select {
	case <-changed:
	default:
		t.Fatal("attached reader was not notified")
	}
	next, n, _, _ := tab.Output(offset, buffer)
	if string(buffer[:n]) != "new output" || next != offset+uint64(n) {
		t.Fatalf("attached read: offset=%d bytes=%q", next, buffer[:n])
	}
	replayEnd, n, _, _ := tab.Output(0, buffer)
	if replayEnd != next || n != terminalHistorySize || !bytes.HasSuffix(buffer[:n], []byte("new output")) {
		t.Fatalf("replay after wrap: offset=%d bytes=%d", replayEnd, n)
	}
}
