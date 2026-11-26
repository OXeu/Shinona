package sshsandbox

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestSSHCommandsAndTerminalsStopWhenTransportStalls(t *testing.T) {
	for _, name := range []string{"command", "watchdog command", "terminal cancellation", "terminal close deadline"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			const cleanup = 100 * time.Millisecond
			workspace, transport, started := sshCancellationWorkspace(t, ctx, cleanup)
			result := make(chan error, 1)
			turn, stop := context.WithCancel(ctx)
			defer stop()
			var terminal sandbox.Terminal
			switch name {
			case "command":
				go func() {
					_, err := workspace.command(turn, nil, 1024, "ps")
					result <- err
				}()
			case "watchdog command":
				go func() {
					_, err := workspace.Exec(turn, sandbox.Command{Program: "sleep"})
					result <- err
				}()
			default:
				var err error
				terminal, err = workspace.OpenTerminal(turn, sandbox.TerminalSize{Cols: 80, Rows: 24})
				if err != nil {
					t.Fatal(err)
				}
			}
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("SSH command did not start")
			}
			transport.stalled.Store(true)
			if name == "terminal close deadline" {
				closeCtx, stopClose := context.WithTimeout(ctx, cleanup/2)
				defer stopClose()
				go func() { result <- terminal.Close(closeCtx) }()
			} else {
				if terminal != nil {
					go func() {
						_, err := terminal.Read(make([]byte, 8))
						result <- err
					}()
				}
				stop()
			}
			select {
			case err := <-result:
				if terminal == nil && !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled command returned %v", err)
				}
				if name == "terminal close deadline" && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("terminal close returned %v", err)
				}
				if name == "terminal cancellation" && err == nil {
					t.Fatal("stalled terminal read returned without an error")
				}
			case <-ctx.Done():
				t.Fatal("stalled SSH transport outlived cleanup")
			}
			if !workspace.discard.Load() {
				t.Fatal("stalled transport could be returned to the idle pool")
			}
		})
	}
}

func TestSSHReadCancellationKeepsResponsiveSharedTransport(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	workspace, _, started := sshCancellationWorkspace(t, ctx, time.Second)
	workspace.shared = true
	request, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := workspace.command(request, nil, 1024, "ps")
		done <- err
	}()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("SSH command did not start")
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read returned %v", err)
		}
	case <-ctx.Done():
		t.Fatal("SSH channel did not close after canceling a read")
	}
	stopProbe := context.AfterFunc(ctx, workspace.abortTransport)
	defer stopProbe()
	if _, _, err := workspace.client.SendRequest("keepalive@openssh.com", true, nil); err != nil {
		t.Fatalf("canceling one read closed its responsive shared transport: %v", err)
	}
}

func TestSSHSharedSessionOpenStopsAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	workspace, transport, _ := sshCancellationWorkspace(t, ctx, 50*time.Millisecond)
	workspace.shared = true
	transport.stalled.Store(true)
	request, stop := context.WithCancel(ctx)
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := workspace.openSession(request)
		done <- err
	}()
	select {
	case <-transport.blocked:
	case <-ctx.Done():
		t.Fatal("SSH channel open did not reach the transport")
	}
	stop()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || !workspace.discard.Load() {
			t.Fatalf("canceled SSH channel open: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("shared SSH channel open did not stop within cleanup")
	}
}

type stalledSSHTransport struct {
	net.Conn
	stalled atomic.Bool
	blocked chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (c *stalledSSHTransport) Write(data []byte) (int, error) {
	if !c.stalled.Load() {
		return c.Conn.Write(data)
	}
	select {
	case c.blocked <- struct{}{}:
	default:
	}
	<-c.closed
	return 0, net.ErrClosed
}

func (c *stalledSSHTransport) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

func sshCancellationWorkspace(t *testing.T, ctx context.Context, cleanup time.Duration) (*workspace, *stalledSSHTransport, <-chan struct{}) {
	t.Helper()
	started := make(chan struct{})
	provider := sshFixtureProvider(t, cleanup, func(_ ssh.Channel, requests <-chan *ssh.Request) {
		for request := range requests {
			switch request.Type {
			case "pty-req":
				_ = request.Reply(true, nil)
			case "exec":
				_ = request.Reply(true, nil)
				close(started)
			default:
				_ = request.Reply(false, nil)
			}
		}
	})
	var transport *stalledSSHTransport
	provider.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		transport = &stalledSSHTransport{Conn: conn, blocked: make(chan struct{}, 1), closed: make(chan struct{})}
		return transport, nil
	}
	workspace, err := provider.connect(ctx, sandbox.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = workspace.Close(t.Context()) })
	return workspace, transport, started
}
