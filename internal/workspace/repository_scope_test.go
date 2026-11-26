package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/OXeu/shinona/internal/sandbox"
)

// localWorkspace runs commands directly on a local directory for tests.
type localWorkspace struct{ dir string }

func (w localWorkspace) ReadFile(_ context.Context, name string) ([]byte, error) {
	return os.ReadFile(filepath.Join(w.dir, name))
}

func (w localWorkspace) WriteFile(_ context.Context, name string, data []byte) error {
	full := filepath.Join(w.dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		return err
	}
	return os.WriteFile(full, data, 0o644)
}

func (w localWorkspace) Exec(ctx context.Context, command sandbox.Command) (sandbox.Result, error) {
	cmd := exec.CommandContext(ctx, command.Program, command.Args...)
	cmd.Dir = w.dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	result := sandbox.Result{Stdout: stdout.String(), Stderr: stderr.String()}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return result, err
	}
	result.ExitCode = exitErr.ExitCode()
	return result, nil
}

func (w localWorkspace) Close(context.Context) error { return nil }

func gitTest(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func write(t *testing.T, dir, name, content string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// scaffold builds a repository with two sibling subtrees and a commit history
// that touches each of them.
func scaffold(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitTest(t, dir, "init", "-q", ".")
	gitTest(t, dir, "config", "user.email", "a@b.c")
	gitTest(t, dir, "config", "user.name", "A")
	write(t, dir, "api/main.go", "package main\n")
	write(t, dir, "web/app.ts", "export {}\n")
	write(t, dir, "top.txt", "top\n")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "-qm", "base")
	write(t, dir, "top.txt", "top2\n")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "-qm", "touch-top-only")
	write(t, dir, "api/main.go", "package main // changed\n")
	gitTest(t, dir, "add", "-A")
	gitTest(t, dir, "commit", "-qm", "touch-api")
	return dir
}

func TestScopedChangesStayInsideTheSelectedDirectory(t *testing.T) {
	ctx := t.Context()
	dir := scaffold(t)
	write(t, dir, "api/extra.go", "package main\n")
	write(t, dir, "web/extra.ts", "export {}\n")
	write(t, dir, "top.txt", "dirty\n")

	changes, err := New(localWorkspace{dir}).At("api").Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	paths := make([]string, len(changes.Entries))
	for i, entry := range changes.Entries {
		paths[i] = entry.Path
	}
	if len(paths) != 1 || paths[0] != "extra.go" {
		t.Fatalf("scoped changes: %v", paths)
	}
}

func TestScopedHistorySkipsCommitsOutsideTheDirectory(t *testing.T) {
	ctx := t.Context()
	dir := scaffold(t)
	history, err := New(localWorkspace{dir}).At("api").History(ctx)
	if err != nil {
		t.Fatal(err)
	}
	subjects := make([]string, len(history.Entries))
	for i, entry := range history.Entries {
		subjects[i] = entry.Subject
	}
	if len(subjects) != 2 || subjects[0] != "touch-api" || subjects[1] != "base" {
		t.Fatalf("scoped history: %v", subjects)
	}
}

func TestRenameAcrossTheScopeBoundaryReportsOneSide(t *testing.T) {
	ctx := t.Context()
	dir := scaffold(t)
	gitTest(t, dir, "mv", "api/main.go", "web/main.go")

	changes, err := New(localWorkspace{dir}).At("api").Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Entries) != 1 {
		t.Fatalf("expected one scoped entry, got %+v", changes.Entries)
	}
	entry := changes.Entries[0]
	if entry.Path != "main.go" || entry.Status != "D" {
		t.Fatalf("rename out of scope: %+v", entry)
	}
}

func TestCommitOnlyTouchesPathsInsideScope(t *testing.T) {
	ctx := t.Context()
	dir := scaffold(t)
	write(t, dir, "api/main.go", "package main // scoped\n")
	write(t, dir, "web/app.ts", "export const changed = 1\n")
	gitTest(t, dir, "add", "-A")

	repo := New(localWorkspace{dir}).At("api")
	changes, err := repo.Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Entries) != 1 || changes.Entries[0].Path != "main.go" {
		t.Fatalf("unexpected scoped changes: %+v", changes.Entries)
	}
	if _, err := repo.Commit(ctx, "a@b.c", "scoped commit", []string{"main.go"}); err != nil {
		t.Fatal(err)
	}
	// The sibling tree must stay staged and uncommitted.
	status, err := New(localWorkspace{dir}).Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Entries) != 1 || status.Entries[0].Path != "web/app.ts" || status.Entries[0].Status != "M" {
		t.Fatalf("outside change was disturbed: %+v", status.Entries)
	}
}

func TestScopedRootRejectsMissingAndEscapingDirectories(t *testing.T) {
	ctx := t.Context()
	dir := scaffold(t)
	repo := New(localWorkspace{dir})
	if err := repo.ResolveRoot(ctx, "api"); err != nil {
		t.Fatalf("existing folder: %v", err)
	}
	if err := repo.ResolveRoot(ctx, "missing"); !errors.Is(err, ErrPath) {
		t.Fatalf("missing folder: %v", err)
	}
	if err := repo.ResolveRoot(ctx, "../outside"); !errors.Is(err, ErrPath) {
		t.Fatalf("escaping folder: %v", err)
	}
	if err := repo.ResolveRoot(ctx, ""); err != nil {
		t.Fatalf("root folder: %v", err)
	}
}

func TestScopedRepositoryRequiresAGitRepository(t *testing.T) {
	ctx := t.Context()
	dir := scaffold(t)
	// Sibling subtrees of a repository are valid scopes.
	if _, err := New(localWorkspace{dir}).At("api").Changes(ctx); err != nil {
		t.Fatalf("nested scope: %v", err)
	}
	if _, err := New(localWorkspace{dir}).At("web").Changes(ctx); err != nil {
		t.Fatalf("nested scope: %v", err)
	}
	// A directory outside any repository is not.
	plain := t.TempDir()
	if err := os.WriteFile(filepath.Join(plain, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(localWorkspace{plain}).Changes(ctx); !errors.Is(err, ErrRepository) {
		t.Fatalf("outside repository: %v", err)
	}
}
