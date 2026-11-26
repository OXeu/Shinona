package workspace

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// The workspace is the repository boundary: never resolve a repository from
	// above it. This also keeps the tests independent of the directory the test
	// binary happens to run in, such as a checkout that is itself a repository.
	cmd.Env = append(os.Environ(), "GIT_CEILING_DIRECTORIES="+filepath.Dir(w.dir))
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

// A project commonly keeps its repository in a subfolder of the sandbox
// workspace. Selecting that folder must work even though it is the top of its
// own work tree, which reports an empty Git prefix.
func TestSelectedDirectoryMayBeTheTopOfItsOwnRepository(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	project := filepath.Join(dir, "myproject")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, project, "init", "-q", ".")
	gitTest(t, project, "config", "user.email", "a@b.c")
	gitTest(t, project, "config", "user.name", "A")
	write(t, project, "committed.txt", "base\n")
	gitTest(t, project, "add", "-A")
	gitTest(t, project, "commit", "-qm", "base")

	write(t, project, "fresh.txt", "new\n")

	repo := New(localWorkspace{dir}).At("myproject")
	changes, err := repo.Changes(ctx)
	if err != nil {
		t.Fatalf("changes at a nested repository root: %v", err)
	}
	if len(changes.Entries) != 1 || changes.Entries[0].Path != "fresh.txt" {
		t.Fatalf("scoped changes: %+v", changes.Entries)
	}

	history, err := repo.History(ctx)
	if err != nil {
		t.Fatalf("history at a nested repository root: %v", err)
	}
	if len(history.Entries) != 1 || history.Entries[0].Subject != "base" {
		t.Fatalf("scoped history: %+v", history.Entries)
	}

	diff, err := repo.Diff(ctx, "committed.txt", history.Entries[0].Hash)
	if err != nil {
		t.Fatalf("diff at a nested repository root: %v", err)
	}
	if !strings.Contains(diff.Text, "committed.txt") {
		t.Fatalf("scoped diff: %q", diff.Text)
	}
}

// A selected folder that is not inside any repository still reports the
// dedicated error so the panel can explain itself.
func TestSelectedDirectoryOutsideEveryRepository(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	write(t, dir, "loose.txt", "x\n")
	if _, err := New(localWorkspace{dir}).At(".").Changes(ctx); !errors.Is(err, ErrRepository) {
		t.Fatalf("expected ErrRepository, got %v", err)
	}
}

// A folder inside a repository that itself lives in a workspace subfolder
// needs both the repository prefix and the scope rebasing together.
func TestSelectedDirectoryInsideANestedRepository(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	project := filepath.Join(dir, "myproject")
	if err := os.MkdirAll(filepath.Join(project, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitTest(t, project, "init", "-q", ".")
	gitTest(t, project, "config", "user.email", "a@b.c")
	gitTest(t, project, "config", "user.name", "A")
	write(t, project, "sub/kept.txt", "base\n")
	write(t, project, "outside.txt", "base\n")
	gitTest(t, project, "add", "-A")
	gitTest(t, project, "commit", "-qm", "base")
	write(t, project, "sub/kept.txt", "changed\n")
	write(t, project, "outside.txt", "changed\n")

	changes, err := New(localWorkspace{dir}).At("myproject/sub").Changes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Entries) != 1 || changes.Entries[0].Path != "kept.txt" {
		t.Fatalf("nested scope changes: %+v", changes.Entries)
	}
}
