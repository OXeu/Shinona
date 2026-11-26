package sqlite

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestWALMaintenanceReusesLogWithoutWriterAutoCheckpoint(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	repo, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })

	var threshold int
	if err := repo.db.WithContext(ctx).Raw("PRAGMA wal_autocheckpoint").Row().Scan(&threshold); err != nil {
		t.Fatal(err)
	}
	if threshold != 0 {
		t.Fatalf("writer autocheckpoint = %d, want 0", threshold)
	}

	logSize := func() int64 {
		t.Helper()
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	size := logSize()
	deadline := time.Now().Add(5 * time.Second)
	for i := 0; time.Now().Before(deadline); i++ {
		time.Sleep(25 * time.Millisecond)
		now := time.Now().UTC()
		id := fmt.Sprintf("checkpoint-%d", i)
		if err := repo.CreateSession(ctx, run.Session{ID: id, OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
			t.Fatal(err)
		}
		after := logSize()
		// Writes append to the log until a checkpoint permits SQLite to reuse it.
		if after <= size {
			return
		}
		size = after
	}
	t.Fatal("the background checkpoint did not let the writer reuse the WAL")
}
