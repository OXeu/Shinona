package sshsandbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/sandbox"
)

func sshFixtureProvider(t *testing.T, cleanup time.Duration, handle func(ssh.Channel, <-chan *ssh.Request)) *Provider {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	server := &ssh.ServerConfig{PublicKeyCallback: func(ssh.ConnMetadata, ssh.PublicKey) (*ssh.Permissions, error) { return nil, nil }}
	server.AddHostKey(signer)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = conn.Close() }()
				_, channels, requests, err := ssh.NewServerConn(conn, server)
				if err != nil {
					return
				}
				go ssh.DiscardRequests(requests)
				for incoming := range channels {
					channel, requests, err := incoming.Accept()
					if err != nil {
						return
					}
					go func() {
						defer func() { _ = channel.Close() }()
						handle(channel, requests)
					}()
				}
			}()
		}
	}()
	connections := &ProjectConnections{}
	t.Cleanup(func() { _ = connections.Close() })
	return &Provider{address: listener.Addr().String(), cleanup: cleanup, dial: (&net.Dialer{}).DialContext, projects: connections, client: &ssh.ClientConfig{
		User: "fixture", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
	}}
}

func TestAcquireRecoversWhenDockerStartedBeforeColdSSHDisconnect(t *testing.T) {
	var dials, checks, starts atomic.Int32
	var transport atomic.Pointer[net.TCPConn]
	provider := sshFixtureProvider(t, 100*time.Millisecond, func(channel ssh.Channel, requests <-chan *ssh.Request) {
		request, ok := <-requests
		if !ok || request.Type != "exec" {
			t.Error("expected a Docker command")
			return
		}
		var exec struct{ Command string }
		if err := ssh.Unmarshal(request.Payload, &exec); err != nil {
			t.Error(err)
			return
		}
		_ = request.Reply(true, nil)
		switch {
		case strings.HasPrefix(exec.Command, "exec docker 'ps' "):
			checks.Add(1)
			state := "exited"
			if starts.Load() != 0 {
				state = "running"
			}
			_, _ = fmt.Fprintf(channel, "fixture-container %s hardened\n", state)
			_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{}))
		case strings.HasPrefix(exec.Command, "exec docker 'start' "):
			starts.Add(1)
			// Docker has applied the operation, but SSH cannot return the status.
			_ = transport.Load().Close()
		default:
			t.Errorf("unexpected Docker command %q", exec.Command)
		}
	})
	provider.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
		conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		dials.Add(1)
		transport.Store(conn.(*net.TCPConn))
		return conn, nil
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	spec := sandbox.Spec{OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared}
	ws, err := provider.Acquire(ctx, spec)
	if err != nil {
		t.Fatalf("resume the started container after SSH disconnect: %v", err)
	}
	if err := ws.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if dials.Load() != 2 || checks.Load() != 2 || starts.Load() != 1 {
		t.Fatalf("acquire performed %d dials, %d status checks and %d starts", dials.Load(), checks.Load(), starts.Load())
	}
}

func TestDestroyProjectVerifiesRemovalAfterSSHDisconnect(t *testing.T) {
	const original = "container-before-disconnect"
	destroy, err := dockerCommand([]string{"rm", "--force", "--volumes", "--", original})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name    string
		removed bool
	}{
		{name: "container removed", removed: true},
		{name: "container still present"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dials, checks, removals atomic.Int32
			var removed atomic.Bool
			var transport atomic.Pointer[net.TCPConn]
			provider := sshFixtureProvider(t, 100*time.Millisecond, func(channel ssh.Channel, requests <-chan *ssh.Request) {
				request, ok := <-requests
				if !ok || request.Type != "exec" {
					t.Error("expected a Docker command")
					return
				}
				var exec struct{ Command string }
				if err := ssh.Unmarshal(request.Payload, &exec); err != nil {
					t.Error(err)
					return
				}
				_ = request.Reply(true, nil)
				switch {
				case strings.HasPrefix(exec.Command, "exec docker 'ps' "):
					checks.Add(1)
					if !removed.Load() {
						_, _ = io.WriteString(channel, original+" running hardened\n")
					}
					_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{}))
				case exec.Command == destroy:
					removals.Add(1)
					removed.Store(tc.removed)
					_ = transport.Load().Close()
				default:
					t.Errorf("unexpected Docker command %q", exec.Command)
				}
			})
			provider.dial = func(ctx context.Context, network, address string) (net.Conn, error) {
				conn, err := (&net.Dialer{}).DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				dials.Add(1)
				transport.Store(conn.(*net.TCPConn))
				return conn, nil
			}
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			spec := sandbox.Spec{OwnerID: "owner", ProjectID: "project"}
			status, err := provider.ProjectStatus(ctx, spec)
			if err != nil || status.ID != original {
				t.Fatalf("initial project status: %+v, %v", status, err)
			}
			err = provider.DestroyProject(ctx, spec, new(original))
			if (err == nil) != tc.removed || dials.Load() != 2 || checks.Load() != 3 || removals.Load() != 1 {
				t.Fatalf("destroy returned %v after %d dials, %d status checks, and %d removals",
					err, dials.Load(), checks.Load(), removals.Load())
			}
		})
	}
}

func TestDestroyProjectTargetsTheInspectedContainer(t *testing.T) {
	original, replacement := strings.Repeat("a", 64), strings.Repeat("b", 64)
	destroy, err := dockerCommand([]string{"rm", "--force", "--volumes", "--", original})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, remaining, selection string
		exitStatus                 uint32
	}{
		{name: "removed", selection: original},
		{name: "already removed", selection: original, exitStatus: 1},
		{name: "replaced after acknowledged removal", selection: original, remaining: replacement},
		{name: "replaced after ambiguous removal", selection: original, remaining: replacement, exitStatus: 1},
		{name: "still present", selection: original, remaining: original, exitStatus: 1},
		{name: "stale selection", selection: replacement},
		{name: "provisioned after an empty selection"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var checks, removals atomic.Int32
			provider := sshFixtureProvider(t, time.Second, func(channel ssh.Channel, requests <-chan *ssh.Request) {
				request, ok := <-requests
				if !ok || request.Type != "exec" {
					t.Error("expected a Docker command")
					return
				}
				var exec struct{ Command string }
				if err := ssh.Unmarshal(request.Payload, &exec); err != nil {
					t.Error(err)
					return
				}
				_ = request.Reply(true, nil)
				var exitStatus uint32
				switch {
				case strings.HasPrefix(exec.Command, "exec docker 'ps' "):
					id := tc.remaining
					if checks.Add(1) == 1 {
						id = original
					}
					if id != "" {
						_, _ = io.WriteString(channel, id+" running hardened\n")
					}
				case exec.Command == destroy:
					removals.Add(1)
					exitStatus = tc.exitStatus
				default:
					t.Errorf("unexpected Docker command: %s", exec.Command)
					exitStatus = 1
				}
				_, _ = channel.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{exitStatus}))
			})
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			err := provider.DestroyProject(ctx, sandbox.Spec{OwnerID: "owner", ProjectID: "project"}, &tc.selection)
			if tc.selection != original {
				if !errors.Is(err, sandbox.ErrProjectChanged) || checks.Load() != 1 || removals.Load() != 0 {
					t.Fatalf("stale selection returned %v after %d status checks and %d removals", err, checks.Load(), removals.Load())
				}
				return
			}
			wantChecks := int32(1)
			if tc.exitStatus != 0 {
				wantChecks++
			}
			if (err != nil) != (tc.exitStatus != 0 && tc.remaining != "") || checks.Load() != wantChecks || removals.Load() != 1 {
				t.Fatalf("destroy returned %v after %d status checks and %d removals", err, checks.Load(), removals.Load())
			}
		})
	}
}

func TestProjectConnectionsReuseMultipleTransportsForTheSameOwner(t *testing.T) {
	provider := sshFixtureProvider(t, time.Second, func(_ ssh.Channel, _ <-chan *ssh.Request) {})
	key := projectConnectionKey{owner: "owner", identity: provider.identity}
	for range maxIdleConnectionsPerKey + 1 {
		client, err := provider.connectClient(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if err := provider.projects.put(key, &projectConnection{client: client, opened: time.Now()}); err != nil {
			t.Fatal(err)
		}
	}
	for range maxIdleConnectionsPerKey {
		conn := provider.projects.take(key)
		if conn == nil {
			t.Fatal("an idle transport was closed instead of retained")
		}
		t.Cleanup(func() { _ = conn.client.Close() })
	}
	if conn := provider.projects.take(key); conn != nil {
		_ = conn.client.Close()
		t.Fatal("pool retained more transports than the per-key limit")
	}
}
