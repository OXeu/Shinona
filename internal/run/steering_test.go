package run

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
)

func TestSteeringRemainsReadableWhileReceivingQueuedMessage(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mailbox Steering
		addSteering(t, &mailbox, "first")
		unblock := make(chan struct{})
		release := sync.OnceFunc(func() { close(unblock) })
		defer release()
		moved := make(chan error, 1)
		go func() {
			moved <- mailbox.enqueue(func() (SteeringMessage, error) {
				<-unblock
				return SteeringMessage{ID: "queued", Text: "queued"}, nil
			})
		}()
		synctest.Wait()
		addSteering(t, &mailbox, "last")
		if messages := mailbox.snapshot(); len(messages) != 2 || messages[0].ID != "first" || messages[1].ID != "last" {
			t.Fatalf("snapshot included an uncommitted message: %+v", messages)
		}
		if delivered := mailbox.Drain(); !slices.Equal(delivered, []string{"first"}) {
			t.Fatalf("checkpoint passed a reserved message: %v", delivered)
		}
		finished := make(chan []string, 1)
		go func() { finished <- mailbox.Finish(t.Context()) }()
		synctest.Wait()
		select {
		case delivered := <-finished:
			t.Fatalf("final checkpoint ended before the transfer: %v", delivered)
		default:
		}
		release()
		if err := <-moved; err != nil {
			t.Fatal(err)
		}
		if delivered := <-finished; !slices.Equal(delivered, []string{"queued", "last"}) {
			t.Fatalf("transfer reordered steering: %v", delivered)
		}
		mailbox.Finish(t.Context())
		if err := mailbox.enqueue(func() (SteeringMessage, error) { return SteeringMessage{}, nil }); !errors.Is(err, ErrNotRunning) {
			t.Fatalf("final checkpoint did not seal the mailbox: %v", err)
		}
	})
}

func TestSteeringQueueTransferKeepsOrRemovesOriginalMessage(t *testing.T) {
	failure := errors.New("fixture queue write failed")
	for _, tc := range []struct {
		name string
		err  error
		want []string
	}{
		{name: "commit", want: []string{"last"}},
		{name: "rollback", err: failure, want: []string{"first", "last"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var mailbox Steering
				addSteering(t, &mailbox, "first")
				addSteering(t, &mailbox, "last")
				unblock := make(chan struct{})
				release := sync.OnceFunc(func() { close(unblock) })
				defer release()
				moved := make(chan error, 1)
				go func() {
					moved <- mailbox.take("first", func(SteeringMessage) error {
						<-unblock
						return tc.err
					})
				}()
				synctest.Wait()
				if delivered := mailbox.Drain(); len(delivered) != 0 {
					t.Fatalf("delivered messages before the transfer settled: %v", delivered)
				}
				if err := mailbox.take("first", func(SteeringMessage) error { return nil }); !errors.Is(err, ErrNotRunning) {
					t.Fatalf("an in-flight message was taken twice: %v", err)
				}
				release()
				if err := <-moved; !errors.Is(err, tc.err) {
					t.Fatalf("queue write returned %v, want %v", err, tc.err)
				}
				if delivered := mailbox.Drain(); !slices.Equal(delivered, tc.want) {
					t.Fatalf("delivery after transfer: %v, want %v", delivered, tc.want)
				}
			})
		})
	}
}

func TestSteeringCompletionIncludesInFlightMessageAfterCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mailbox Steering
		unblock := make(chan struct{})
		release := sync.OnceFunc(func() { close(unblock) })
		defer release()
		moved := make(chan error, 1)
		go func() {
			moved <- mailbox.enqueue(func() (SteeringMessage, error) {
				<-unblock
				return SteeringMessage{ID: "queued", Text: "queued"}, nil
			})
		}()
		synctest.Wait()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		finished := make(chan []string, 1)
		go func() { finished <- mailbox.Finish(ctx) }()
		synctest.Wait()
		cancel()
		if delivered := <-finished; len(delivered) != 0 {
			t.Fatalf("canceled checkpoint delivered messages: %v", delivered)
		}
		completed := make(chan []SteeringMessage, 1)
		go func() { completed <- mailbox.close() }()
		synctest.Wait()
		select {
		case messages := <-completed:
			t.Fatalf("persisting completion lost the pending transfer: %+v", messages)
		default:
		}
		release()
		if err := <-moved; err != nil {
			t.Fatal(err)
		}
		if messages := <-completed; len(messages) != 1 || messages[0].ID != "queued" || messages[0].Delivered {
			t.Fatalf("completion lost the queued message or marked it delivered: %+v", messages)
		}
	})
}

func addSteering(t *testing.T, mailbox *Steering, text string) {
	t.Helper()
	if err := mailbox.enqueue(func() (SteeringMessage, error) {
		return SteeringMessage{ID: text, Text: text}, nil
	}); err != nil {
		t.Fatal(err)
	}
}
