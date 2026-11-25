package e2b

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

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
		workspace.keepAwake(t.Context(), awakeWindow)
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
