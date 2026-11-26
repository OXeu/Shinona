package keylock

import (
	"sync"
	"testing"
)

func TestReleaseIsIdempotent(t *testing.T) {
	var locks Locks[string]
	first, err := locks.Acquire(t.Context(), "project", false)
	if err != nil {
		t.Fatal(err)
	}
	var releases sync.WaitGroup
	for range 4 {
		releases.Go(first)
	}
	releases.Wait()
	second, ok := locks.TryAcquire("project", false)
	if !ok {
		t.Fatal("the key remained locked after release")
	}
	first()
	if acquire, ok := locks.TryAcquire("project", false); ok {
		acquire()
		t.Fatal("releasing a stale lease unlocked the current holder")
	}
	second()
	second()
	if len(locks.entries) != 0 {
		t.Fatal("released keys remained registered")
	}
}
