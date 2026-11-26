package run

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestInspectWorkspaceBorrowsOnlyTheSessionsLiveLease(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	active := &activeTurn{item: Run{OwnerID: "owner", SessionID: "session"}, ctx: ctx}
	controls := &Controls{active: active}
	controls.leaseMu.Lock()
	defer controls.leaseMu.Unlock()
	service := NewService(nil, nil, nil, controls, nil)
	unexpectedUse := func(context.Context, sandbox.Workspace) error {
		t.Fatal("inspected an unavailable workspace")
		return nil
	}
	if err := service.InspectWorkspace(ctx, "owner", "session", unexpectedUse); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("inspect during provisioning: %v", err)
	}

	box := &stubWorkspace{}
	detach := controls.publishWorkspace(active, box)
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	read := make(chan error, 1)
	go func() {
		read <- service.InspectWorkspace(ctx, "owner", "session", func(ctx context.Context, ws sandbox.Workspace) error {
			defer close(canceled)
			close(entered)
			if ws != box {
				return errors.New("inspection used an unexpected workspace")
			}
			<-ctx.Done()
			return ctx.Err()
		})
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("inspection could not borrow the active workspace")
	}
	for _, other := range []sessionKey{{owner: "other", id: "session"}, {owner: "owner", id: "other"}} {
		if err := service.InspectWorkspace(ctx, other.owner, other.id, unexpectedUse); !errors.Is(err, ErrWorkspaceBusy) {
			t.Fatalf("inspect another session: %v", err)
		}
	}
	if err := service.WithWorkspace(ctx, "owner", "session", unexpectedUse); !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("mutate during a turn: %v", err)
	}
	go func() {
		detach()
		close(release)
	}()
	select {
	case <-release:
		select {
		case <-canceled:
		default:
			t.Fatal("lease detached while inspection was still using it")
		}
	case <-ctx.Done():
		t.Fatal("lease did not cancel the inspection before detaching")
	}
	if err := <-read; !errors.Is(err, ErrWorkspaceBusy) {
		t.Fatalf("inspect during teardown: %v", err)
	}
}

type stubWorkspace struct{ sandbox.Workspace }
