package sshsandbox

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
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
		conn, err := listener.Accept()
		if err != nil {
			return
		}
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
	return &Provider{address: listener.Addr().String(), cleanup: cleanup, dial: (&net.Dialer{}).DialContext, client: &ssh.ClientConfig{
		User: "fixture", Auth: []ssh.AuthMethod{ssh.PublicKeys(signer)}, HostKeyCallback: ssh.FixedHostKey(signer.PublicKey()),
	}}
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
		{name: "replaced", selection: original, remaining: replacement},
		{name: "still present", selection: original, remaining: original, exitStatus: 1},
		{name: "stale selection", selection: replacement},
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
			err := provider.DestroyProject(ctx, sandbox.Spec{OwnerID: "owner", ProjectID: "project"}, tc.selection)
			if tc.selection != original {
				if !errors.Is(err, sandbox.ErrProjectChanged) || checks.Load() != 1 || removals.Load() != 0 {
					t.Fatalf("stale selection returned %v after %d status checks and %d removals", err, checks.Load(), removals.Load())
				}
				return
			}
			if (err != nil) != (tc.remaining != "") || checks.Load() != 2 || removals.Load() != 1 {
				t.Fatalf("destroy returned %v after %d status checks and %d removals", err, checks.Load(), removals.Load())
			}
		})
	}
}
