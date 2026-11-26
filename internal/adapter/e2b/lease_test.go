package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestReconnectingWithNewTimeoutUpdatesActiveRenewals(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		store := openStore(t, ":memory:")
		seedWorkspaceOwner(t, store, "owner", "project")
		var windows []int
		client := &http.Client{Transport: transportFunc(func(r *http.Request) (*http.Response, error) {
			response := &http.Response{StatusCode: http.StatusNoContent, Header: make(http.Header), Body: http.NoBody}
			switch r.URL.Path {
			case "/sandboxes", "/sandboxes/box/connect":
				response.StatusCode = http.StatusOK
				if r.URL.Path == "/sandboxes" {
					response.StatusCode = http.StatusCreated
				}
				response.Header.Set("Content-Type", "application/json")
				response.Body = io.NopCloser(strings.NewReader(`{"sandboxID":"box","envdVersion":"0.11.0","envdAccessToken":"fixture"}`))
			case "/sandboxes/box/timeout":
			case "/sandboxes/box/refreshes":
				var body struct{ Duration int }
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					return nil, err
				}
				windows = append(windows, body.Duration)
			default:
				return nil, fmt.Errorf("unexpected E2B path %s", r.URL.Path)
			}
			return response, nil
		})}
		config := Config{APIKey: "fixture", BaseURL: "https://fixture.test", APIVersion: "v1", WorkDirectory: "/workspace",
			ProjectLeases: &ProjectLeases{}}
		provider, err := New(config, client, store.E2B)
		if err != nil {
			t.Fatal(err)
		}
		ctx := t.Context()
		spec := sandbox.Spec{OwnerID: "owner", ProjectID: "project", Policy: sandbox.Shared}
		terminal, err := provider.Acquire(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = terminal.Close(ctx) }()
		checkRenewals := func(after time.Duration, want ...int) {
			t.Helper()
			time.Sleep(after)
			synctest.Wait()
			if !slices.Equal(windows, want) {
				t.Fatalf("renewed with windows %v, want %v", windows, want)
			}
			windows = nil
		}
		checkRenewals(renewEvery+time.Millisecond, 300)
		updated := provider.WithIdleTimeout(sandbox.IdleTimeout{After: 3 * time.Second})
		inspector, err := updated.OpenProject(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := inspector.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkRenewals(2*time.Second+time.Millisecond, 3, 3)
		turn, err := updated.Acquire(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = turn.Close(ctx) }()
		checkRenewals(time.Second+time.Millisecond, 3)
		if err := turn.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkRenewals(time.Second+time.Millisecond, 3)
		config.Timeout = 6 * time.Second
		reloaded, err := New(config, client, store.E2B)
		if err != nil {
			t.Fatal(err)
		}
		inspector, err = reloaded.OpenProject(ctx, spec)
		if err != nil {
			t.Fatal(err)
		}
		if err := inspector.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkRenewals(2*time.Second+time.Millisecond, 6)
		if err := terminal.Close(ctx); err != nil {
			t.Fatal(err)
		}
		checkRenewals(renewEvery)
	})
}

type leaseTestWorkspace struct{ sandbox.Workspace }

func (leaseTestWorkspace) Close(context.Context) error { return nil }

func TestActiveWorkspaceRenewsUntilClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls int
		workspace := projectWorkspace{
			Workspace: leaseTestWorkspace{},
			refresh: func(ctx context.Context, window time.Duration) error {
				if deadline, ok := ctx.Deadline(); !ok || time.Until(deadline) != apiTimeout || window != awakeWindow {
					t.Errorf("renewal deadline %v or window %v is incorrect", deadline, window)
				}
				calls++
				if calls == 6 {
					<-ctx.Done()
					return ctx.Err()
				}
				return nil
			},
		}
		var leases ProjectLeases
		leases.retain(t.Context(), leaseKey{}, &workspace, awakeWindow)
		time.Sleep(6*renewEvery + time.Second)
		synctest.Wait()
		if calls != 6 {
			t.Errorf("renewed %d times in six minutes, want 6", calls)
		}
		if err := workspace.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * renewEvery)
		if calls != 6 {
			t.Errorf("closed workspace continued renewing: %d calls", calls)
		}
	})
}
