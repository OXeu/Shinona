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

func TestTerminalStartupValidationStopsWithService(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const owner, session = "owner", "session"
		repo := workspaceTestRepository{projects: map[sessionKey]string{{owner, session}: ""}}
		resolve := func(ctx context.Context, _, _ string) (Target, error) {
			<-ctx.Done()
			return Target{}, ctx.Err()
		}
		service := NewService(ServiceStores{Sessions: repo, Projects: repo}, resolve, nil, &Controls{}, nil, DefaultWorkspaceTimeouts())
		lifetime, stop := context.WithCancel(t.Context())
		defer stop()
		started := make(chan error, 1)
		go func() {
			_, err := service.StartTerminal(t.Context(), lifetime, owner, session)
			started <- err
		}()
		synctest.Wait()
		stop()
		synctest.Wait()
		select {
		case err := <-started:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("start returned %v", err)
			}
		default:
			t.Fatal("provider resolution still running after service cancellation")
		}
	})
}

func TestTerminalValidationStopsForInvalidatedSessions(t *testing.T) {
	for _, action := range []string{"archive", "delete account"} {
		t.Run(action, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				const owner, other, session = "owner", "other", "session"
				repo := archivingTerminalRepository{workspaceTestRepository{projects: map[sessionKey]string{
					{owner, session}: "", {other, session}: "",
				}}}
				service := NewService(ServiceStores{Sessions: repo, Projects: repo}, func(ctx context.Context, _, _ string) (Target, error) {
					<-ctx.Done()
					return Target{}, ctx.Err()
				}, nil, &Controls{}, nil, DefaultWorkspaceTimeouts())
				lifetime, stop := context.WithCancel(t.Context())
				defer stop()
				start := func(owner string) <-chan error {
					result := make(chan error, 1)
					go func() {
						_, err := service.StartTerminal(t.Context(), lifetime, owner, session)
						result <- err
					}()
					return result
				}
				ownedStart, otherStart := start(owner), start(other)
				synctest.Wait()
				if action == "archive" {
					if err := service.ArchiveSession(t.Context(), owner, session); err != nil {
						t.Fatal(err)
					}
				} else {
					service.AccountDeleted(owner)
				}
				synctest.Wait()
				select {
				case err := <-ownedStart:
					if !errors.Is(err, context.Canceled) {
						t.Fatalf("invalidated start returned %v", err)
					}
				default:
					t.Error("invalidated workspace validation was still running")
				}
				select {
				case err := <-otherStart:
					t.Fatalf("unrelated workspace validation was stopped: %v", err)
				default:
				}
			})
		})
	}
}

func TestTerminalShutdownDrainsWorkspaceValidation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const owner, session = "owner", "session"
		repo := workspaceTestRepository{projects: map[sessionKey]string{{owner, session}: ""}}
		resolving := make(chan context.Context, 1)
		release := make(chan struct{})
		service := NewService(ServiceStores{Sessions: repo, Projects: repo}, func(ctx context.Context, _, _ string) (Target, error) {
			resolving <- ctx
			<-release
			return Target{}, ctx.Err()
		}, nil, &Controls{}, nil, DefaultWorkspaceTimeouts())
		done := make(chan error, 1)
		go func() {
			_, err := service.StartTerminal(t.Context(), t.Context(), owner, session)
			done <- err
		}()
		validation := <-resolving
		shutdown, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		if err := service.CloseTerminals(shutdown); !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("shutdown did not wait for terminal validation: %v", err)
		}
		if !errors.Is(validation.Err(), context.Canceled) {
			t.Errorf("shutdown did not cancel terminal validation: %v", validation.Err())
		}
		close(release)
		if err := service.CloseTerminals(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("validated start returned %v", err)
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
				_, _, state, _ := entry.Output(0, nil, nil)
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
	offset, n, state, changed := tab.Output(0, buffer, nil)
	if offset != terminalHistorySize || n != terminalHistorySize || state != TerminalRunning {
		t.Fatalf("initial replay: offset=%d bytes=%d state=%s", offset, n, state)
	}
	tab.appendOutput([]byte("new output"))
	select {
	case <-changed:
	default:
		t.Fatal("attached reader was not notified")
	}
	next, n, _, _ := tab.Output(offset, buffer, nil)
	if string(buffer[:n]) != "new output" || next != offset+uint64(n) {
		t.Fatalf("attached read: offset=%d bytes=%q", next, buffer[:n])
	}
	replayEnd, n, _, _ := tab.Output(0, buffer, nil)
	if replayEnd != next || n != terminalHistorySize || !bytes.HasSuffix(buffer[:n], []byte("new output")) {
		t.Fatalf("replay after wrap: offset=%d bytes=%d", replayEnd, n)
	}
}

func TestTerminalReplayStopsAtBoundaryWhenHistoryWraps(t *testing.T) {
	tab := TerminalSession{state: TerminalRunning, changed: make(chan struct{})}
	chunk := bytes.Repeat([]byte{'x'}, sandbox.TerminalChunkSize)
	for range terminalHistorySize / len(chunk) {
		tab.appendOutput(chunk)
	}
	boundary := tab.ReplayEnd()
	tab.appendOutput([]byte("live"))
	buffer := make([]byte, terminalHistorySize)
	next, n, _, _ := tab.Output(0, buffer, &boundary)
	if next != boundary || n != terminalHistorySize-len("live") || !bytes.Equal(buffer[:n], bytes.Repeat([]byte{'x'}, n)) {
		t.Fatalf("partial replay crossed its boundary: offset=%d bytes=%d", next, n)
	}
	for range terminalHistorySize / len(chunk) {
		tab.appendOutput(chunk)
	}
	next, n, _, _ = tab.Output(0, buffer, &boundary)
	if next != boundary+uint64(len("live")) || n != 0 {
		t.Fatalf("overwritten replay returned live bytes: offset=%d bytes=%d", next, n)
	}
	_, n, _, _ = tab.Output(next, buffer, nil)
	if n != terminalHistorySize {
		t.Fatalf("live relay could not resume at the oldest retained byte: bytes=%d", n)
	}
}
