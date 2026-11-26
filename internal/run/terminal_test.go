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
		}, nil, &Controls{}, nil)
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
		}, &Controls{}, nil)
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
