package workspace

import (
	"context"
	"errors"
	"fmt"
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
	stdout, stderr := sandbox.NewOutput(command.OutputLimit()), sandbox.NewOutput(sandbox.DefaultOutputLimit)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	err := cmd.Run()
	result := sandbox.Result{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.Truncated() || stderr.Truncated()}
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

func TestCommitCanSelectAPathBeyondTheStatusLimit(t *testing.T) {
	const selected = "zz-selected\n.txt"
	for _, change := range []string{"untracked", "rename"} {
		t.Run(change, func(t *testing.T) {
			dir := scaffold(t)
			for i := range 350 {
				write(t, dir, fmt.Sprintf("api/%03d-%s", i, strings.Repeat("x", 200)), "")
			}
			if change == "rename" {
				gitTest(t, dir, "add", "api")
				gitTest(t, dir, "mv", "api/main.go", "api/"+selected)
			} else {
				write(t, dir, "api/"+selected, "selected\n")
			}
			repo := New(localWorkspace{dir}).At("api")
			changePreview, err := repo.status(t.Context(), sandbox.MaxFileBytes, ".")
			if err != nil || !changePreview.Truncated {
				t.Fatalf("expected truncated status, got %d bytes and error %v", len(changePreview.Stdout), err)
			}
			if hash, err := repo.Commit(t.Context(), "a@b.c", "select one file", []string{selected}); err != nil || hash == "" {
				t.Fatalf("commit returned %q, %v", hash, err)
			}
			if change == "rename" {
				result, err := repo.git(t.Context(), "diff-tree", "-r", "-M", "--no-commit-id", "--name-status", "-z", "HEAD")
				want := "R100\x00api/main.go\x00api/" + selected + "\x00"
				if err != nil || result.ExitCode != 0 || result.Stdout != want {
					t.Fatalf("commit did not include both sides of the rename: %+v, %v", result, err)
				}
			}
		})
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

func TestWorkspaceBrowsingRegularFilesDoesNotRequireRealpath(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "api/file.txt", "inside")
	commands := t.TempDir()
	for _, name := range []string{"sh", "dd"} {
		program, err := exec.LookPath(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(program, filepath.Join(commands, name)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", commands)
	repo := New(localWorkspace{dir})
	if err := repo.ResolveRoot(t.Context(), "api"); err != nil {
		t.Fatal(err)
	}
	list, err := repo.Directory(t.Context(), "api")
	if err != nil || len(list.Entries) != 1 || list.Entries[0].Path != "api/file.txt" {
		t.Fatalf("directory with no realpath on PATH: %+v, %v", list, err)
	}
	file, err := repo.File(t.Context(), "api/file.txt")
	if err != nil || file.Truncated || file.Text != "inside" {
		t.Fatalf("regular file with no realpath on PATH: %+v, %v", file, err)
	}
}

func TestWorkspacePreviewFollowsOnlyInternalFileLinks(t *testing.T) {
	dir, outside := t.TempDir(), t.TempDir()
	write(t, dir, "src/plain.txt", "inside")
	write(t, outside, "plain.txt", "outside")
	for name, target := range map[string]string{
		"inside":  "src/plain.txt",
		"outside": filepath.Join(outside, "plain.txt"),
	} {
		if err := os.Symlink(target, filepath.Join(dir, name)); err != nil {
			t.Fatal(err)
		}
	}
	repo := New(localWorkspace{dir})
	result, err := repo.File(t.Context(), "inside")
	if err != nil || result.Text != "inside" {
		t.Fatalf("preview of a link inside the workspace: %+v, %v", result, err)
	}
	if result, err := repo.File(t.Context(), "outside"); !errors.Is(err, ErrPath) {
		t.Fatalf("preview of a link outside the workspace: %+v, %v", result, err)
	}
}

func TestGitScopePreservesWhitespaceInTheSelectedDirectory(t *testing.T) {
	dir := scaffold(t)
	const scope = " api\n"
	write(t, dir, scope+"/changed.txt", "update")
	repo := New(localWorkspace{dir}).At(scope)
	changes, err := repo.Changes(t.Context())
	if err != nil || len(changes.Entries) != 1 || changes.Entries[0].Path != "changed.txt" {
		t.Fatalf("changes in a directory containing whitespace: %+v, %v", changes, err)
	}
}

func TestGitOperationsRecheckThePhysicalWorkspaceDirectory(t *testing.T) {
	dir := scaffold(t)
	outside := scaffold(t)
	link := filepath.Join(dir, "selected")
	if err := os.Symlink("api", link); err != nil {
		t.Fatal(err)
	}
	repo := New(localWorkspace{dir})
	if err := repo.ResolveRoot(t.Context(), "selected"); err != nil {
		t.Fatalf("selecting an internal directory link: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := repo.At("selected").Changes(t.Context()); !errors.Is(err, ErrRepository) {
		t.Fatalf("retargeted directory link: %v", err)
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
