package sqlite

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/OXeu/shinona/internal/run"
)

func TestWALReplacementWriterDisablesAutoCheckpoint(t *testing.T) {
	ctx := t.Context()
	repo, err := Open(ctx, filepath.Join(t.TempDir(), "replacement.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	pool, err := repo.db.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxIdleConns(0)
	before := pool.Stats().MaxIdleClosed
	for range 2 {
		var threshold int
		if err := pool.QueryRowContext(ctx, "PRAGMA wal_autocheckpoint").Scan(&threshold); err != nil {
			t.Fatal(err)
		}
		if threshold != 0 {
			t.Fatalf("replacement writer autocheckpoint = %d, want 0", threshold)
		}
	}
	if closed := pool.Stats().MaxIdleClosed - before; closed != 2 {
		t.Fatalf("closed %d replacement connections, want 2", closed)
	}
}

func TestWALMaintenanceReusesLogWithoutWriterAutoCheckpoint(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	repo, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	seedAccounts(t, repo, "owner")

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
		// Leave a full checkpoint interval without writes. PASSIVE does not
		// wait for the writer, so a probe on every tick can miss progress.
		time.Sleep(2 * walCheckpointInterval)
		now := time.Now().UTC()
		id := fmt.Sprintf("checkpoint-%d", i)
		if err := repo.Conversations.CreateSession(ctx, run.Session{ID: id, OwnerID: "owner", CreatedAt: now, UpdatedAt: now}); err != nil {
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

func TestWALMaintenanceTruncatesIdleLogBelowWarningThreshold(t *testing.T) {
	const pages = 1024
	testWALMaintenanceTruncatesAfterReaderReleases(t, pages)
}

func TestWALMaintenanceTruncatesLargeLogAfterReaderReleases(t *testing.T) {
	testWALMaintenanceTruncatesAfterReaderReleases(t, walLargeLogPages+64)
}

func testWALMaintenanceTruncatesAfterReaderReleases(t *testing.T, pages int) {
	t.Helper()
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "large-checkpoint.db")
	repo, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.db.Exec("CREATE TABLE wal_fixture (content BLOB)").Error; err != nil {
		t.Fatal(err)
	}
	var pageSize int
	if err := repo.db.Raw("PRAGMA page_size").Row().Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	reader := repo.read.Begin()
	if reader.Error != nil {
		t.Fatal(reader.Error)
	}
	t.Cleanup(func() { _ = reader.Rollback().Error })
	var count int
	if err := reader.Raw("SELECT count(*) FROM wal_fixture").Row().Scan(&count); err != nil {
		t.Fatal(err)
	}

	logSize := func() int64 {
		t.Helper()
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	size := pageSize * pages
	if err := repo.db.Exec("INSERT INTO wal_fixture VALUES (zeroblob(?))", size).Error; err != nil {
		t.Fatal(err)
	}
	got := logSize()
	if got < int64(size) {
		t.Fatalf("WAL held %d bytes with an older snapshot open, expected at least %d", got, size)
	}
	if pages < walLargeLogPages {
		largeLogBytes := walHeaderBytes + walLargeLogPages*(pageSize+walFrameHeaderBytes)
		if got <= walIdleReclaimBytes || got >= int64(largeLogBytes) {
			t.Fatalf("WAL size %d is outside the idle reclaim range (%d, %d)", got, walIdleReclaimBytes, largeLogBytes)
		}
	}
	if err := reader.Rollback().Error; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if logSize() == 0 {
			return
		}
		time.Sleep(walCheckpointInterval / 4)
	}
	t.Fatalf("background checkpoint left %d bytes in the WAL after the reader released", logSize())
}

func TestWALMaintenanceTruncatesReusedLargeLog(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reused checkpoint.db")
	filename := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path)}).String()
	repo, err := Open(ctx, filename)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	// Set up both WAL cycles before the background checkpoint can run again.
	if err := repo.stopCheckpoint(); err != nil {
		t.Fatal(err)
	}
	repo.stopCheckpoint = nil
	if err := repo.db.Exec("CREATE TABLE wal_fixture (content BLOB)").Error; err != nil {
		t.Fatal(err)
	}
	var pageSize int
	if err := repo.db.Raw("PRAGMA page_size").Row().Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	largeWrite := pageSize * (walLargeLogPages + 64)
	if err := repo.db.Exec("INSERT INTO wal_fixture VALUES (zeroblob(?))", largeWrite).Error; err != nil {
		t.Fatal(err)
	}
	checkpoint := func() int {
		t.Helper()
		var busy, pages, checkpointed int
		if err := repo.db.Raw("PRAGMA wal_checkpoint(PASSIVE)").Row().Scan(&busy, &pages, &checkpointed); err != nil {
			t.Fatal(err)
		}
		if busy != 0 || pages != checkpointed {
			t.Fatalf("checkpoint left outstanding work: busy=%d, pages=%d, checkpointed=%d", busy, pages, checkpointed)
		}
		return pages
	}
	if pages := checkpoint(); pages < walLargeLogPages {
		t.Fatalf("large write used only %d WAL pages", pages)
	}
	if err := repo.db.Exec("INSERT INTO wal_fixture VALUES ('small write')").Error; err != nil {
		t.Fatal(err)
	}
	if pages := checkpoint(); pages <= 0 || pages >= walLargeLogPages {
		t.Fatalf("small write used %d WAL pages after recycling", pages)
	}
	logSize := func() int64 {
		t.Helper()
		info, err := os.Stat(path + "-wal")
		if err != nil {
			t.Fatal(err)
		}
		return info.Size()
	}
	if got := logSize(); got < int64(largeWrite) {
		t.Fatalf("recycled WAL held %d bytes, expected at least %d", got, largeWrite)
	}
	_, readDSN, err := databaseDSNs(filename)
	if err != nil {
		t.Fatal(err)
	}
	repo.stopCheckpoint, err = startWALCheckpoint(ctx, readDSN)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if logSize() == 0 {
			return
		}
		time.Sleep(walCheckpointInterval / 4)
	}
	t.Fatalf("background checkpoint left %d bytes in the recycled WAL", logSize())
}
