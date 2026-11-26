// Package workspace reads and commits repository content inside a sandbox lease.
package workspace

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/sandbox"
)

var (
	ErrRepository = errors.New("the workspace root does not contain a Git repository")
	ErrPath       = errors.New("workspace path is invalid or unavailable")
	ErrBinary     = errors.New("binary files cannot be previewed")
	ErrCommit     = errors.New("the selected changes could not be committed")
)

type Change struct {
	Path      string
	Status    string
	Untracked bool
	Original  string
}

type Changes struct {
	Branch    string
	Entries   []Change
	Truncated bool
}

type Commit struct {
	Hash    string
	Short   string
	Author  string
	Date    string
	Subject string
}

type History struct {
	Entries   []Commit
	Truncated bool
}

type Entry struct {
	Path      string
	Name      string
	Directory bool
}

type Directory struct {
	Entries   []Entry
	Truncated bool
}

type Preview struct {
	Text      string
	Truncated bool
}

// Repository reads and commits content under a working directory. The root is
// a workspace-relative directory that scopes changes, history, and paths to the
// directory the user selected as their workspace.
type Repository struct {
	workspace sandbox.Workspace
	root      string
}

func New(ws sandbox.Workspace) Repository { return Repository{workspace: ws} }

// At scopes the repository to a workspace-relative directory. An empty root
// keeps the workspace root.
func (r Repository) At(root string) Repository {
	r.root = root
	return r
}

// Workspace returns the underlying workspace so callers can inspect the whole
// workspace, such as the files panel browsing to choose a directory.
func (r Repository) Workspace() sandbox.Workspace { return r.workspace }

// scoped joins a path relative to the scoped directory.
func (r Repository) scoped(name string) string { return path.Join(r.root, name) }

// workTree reports the scoped directory relative to the top of its Git work
// tree. The prefix is empty when the selected directory is the work tree top,
// which happens when a project keeps its repository in a workspace subfolder.
// ErrRepository reports a directory outside every work tree.
func (r Repository) workTree(ctx context.Context) (string, error) {
	result, err := r.git(ctx, "rev-parse", "--is-inside-work-tree", "--show-prefix")
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", ErrRepository
	}
	lines := strings.Split(strings.TrimRight(result.Stdout, "\n"), "\n")
	if strings.TrimSpace(lines[0]) != "true" {
		return "", ErrRepository
	}
	if len(lines) < 2 {
		return "", nil
	}
	return strings.TrimSpace(lines[1]), nil
}

// relative converts a work-tree path to the scoped directory. The second result
// is false when the path falls outside the scope.
func relative(prefix, name string) (string, bool) {
	if prefix == "" {
		return name, true
	}
	return strings.CutPrefix(name, prefix)
}

func (r Repository) git(ctx context.Context, args ...string) (sandbox.Result, error) {
	options := []string{"--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false", "-c", "color.ui=false"}
	if r.root != "" {
		options = append(options, "-C", r.root)
	}
	return r.workspace.Exec(ctx, sandbox.Command{Program: "/usr/bin/git", Args: append(options, args...)})
}

func (r Repository) requireGit(ctx context.Context) error {
	_, err := r.workTree(ctx)
	return err
}

func (r Repository) Changes(ctx context.Context) (Changes, error) {
	prefix, err := r.workTree(ctx)
	if err != nil {
		return Changes{}, err
	}
	branch, err := r.git(ctx, "symbolic-ref", "--quiet", "--short", "HEAD")
	if err != nil {
		return Changes{}, err
	}
	// The pathspec limits status to the scoped directory, so renames that cross
	// its boundary report as an addition or deletion on this side only.
	status, err := r.git(ctx, "--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--", ".")
	if err != nil {
		return Changes{}, err
	}
	if status.ExitCode != 0 {
		return Changes{}, ErrRepository
	}
	result := Changes{Branch: strings.TrimSpace(branch.Stdout), Entries: []Change{}, Truncated: status.Truncated}
	fields := terminatedFields(status.Stdout)
	for i := 0; i < len(fields); i++ {
		field := fields[i]
		if len(field) < 4 {
			continue
		}
		entry := Change{Path: field[3:], Status: strings.TrimSpace(field[:2]), Untracked: field[:2] == "??"}
		if strings.ContainsAny(entry.Status, "RC") {
			i++
			if i >= len(fields) {
				break
			}
			if strings.Contains(entry.Status, "R") {
				entry.Original = fields[i]
			}
		}
		// Status reports paths from the repository root. Convert them to the
		// scoped directory and drop anything the pathspec let through anyway.
		var ok bool
		if entry.Path, ok = relative(prefix, entry.Path); !ok {
			continue
		}
		if entry.Original != "" {
			if entry.Original, ok = relative(prefix, entry.Original); !ok {
				entry.Original = ""
			}
		}
		if validPath(entry.Path) {
			result.Entries = append(result.Entries, entry)
		}
	}
	return result, nil
}

func (r Repository) History(ctx context.Context) (History, error) {
	if err := r.requireGit(ctx); err != nil {
		return History{}, err
	}
	head, err := r.git(ctx, "rev-parse", "--quiet", "--verify", "HEAD")
	if err != nil {
		return History{}, err
	}
	result := History{Entries: []Commit{}}
	if head.ExitCode != 0 {
		return result, nil
	}
	log, err := r.git(ctx, "log", "-z", "-n", "40", "--format=%H%x00%h%x00%an%x00%aI%x00%s", "--", ".")
	if err != nil {
		return History{}, err
	}
	if log.ExitCode != 0 {
		return History{}, ErrRepository
	}
	fields := terminatedFields(log.Stdout)
	for i := 0; i+4 < len(fields); i += 5 {
		result.Entries = append(result.Entries, Commit{
			Hash: fields[i], Short: fields[i+1], Author: fields[i+2], Date: fields[i+3], Subject: fields[i+4],
		})
	}
	result.Truncated = log.Truncated
	return result, nil
}

// resolvePath follows symbolic links only within the workspace, using a fixed
// script so input paths are passed as arguments and never evaluated as shell.
const resolvePath = `set -eu
root=$(pwd -P)
target=$(realpath -e -- "$1") || exit 2
case "$target" in "$root"|"$root"/*) ;; *) exit 2 ;; esac
case "$target" in */.git|*/.git/*) exit 2 ;; esac
`

func (r Repository) Directory(ctx context.Context, folder string) (Directory, error) {
	if folder != "." && !validPath(folder) {
		return Directory{}, ErrPath
	}
	result, err := r.workspace.Exec(ctx, sandbox.Command{Program: "/bin/bash", Args: []string{
		"-c", resolvePath + `cd -- "$target"
find . -mindepth 1 -maxdepth 1 ! -name .git \( -type d -o -type f \) -printf '%y\0%f\0'`, "shinona-list", folder,
	}})
	if err != nil {
		return Directory{}, err
	}
	if result.ExitCode != 0 {
		return Directory{}, ErrPath
	}
	list := Directory{Entries: []Entry{}, Truncated: result.Truncated}
	fields := terminatedFields(result.Stdout)
	for i := 0; i+1 < len(fields); i += 2 {
		name := fields[i+1]
		entry := path.Join(folder, name)
		if validPath(entry) {
			list.Entries = append(list.Entries, Entry{Path: entry, Name: name, Directory: fields[i] == "d"})
		}
	}
	slices.SortFunc(list.Entries, func(a, b Entry) int {
		if a.Directory != b.Directory {
			if a.Directory {
				return -1
			}
			return 1
		}
		return strings.Compare(a.Name, b.Name)
	})
	return list, nil
}

// ResolveRoot verifies that a workspace-relative directory exists, stays inside
// the workspace, and is not the Git metadata directory.
func (r Repository) ResolveRoot(ctx context.Context, root string) error {
	if root == "" || root == "." {
		return nil
	}
	if !validPath(root) {
		return ErrPath
	}
	result, err := r.workspace.Exec(ctx, sandbox.Command{Program: "/bin/bash", Args: []string{
		"-c", resolvePath + `[ -d "$target" ]`, "shinona-root", root,
	}})
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return ErrPath
	}
	return nil
}

// File previews a workspace-relative file, so the files panel can browse the
// whole workspace and choose a directory.
func (r Repository) File(ctx context.Context, name string) (Preview, error) {
	if !validPath(name) {
		return Preview{}, ErrPath
	}
	return r.file(ctx, name)
}

func (r Repository) file(ctx context.Context, name string) (Preview, error) {
	result, err := r.workspace.Exec(ctx, sandbox.Command{Program: "/bin/bash", Args: []string{
		"-c", resolvePath + `[ -f "$target" ] && head -c 16385 -- "$target"`, "shinona-file", name,
	}})
	return preview(result, err, ErrPath)
}

func (r Repository) Diff(ctx context.Context, name, revision string) (Preview, error) {
	if err := r.requireGit(ctx); err != nil {
		return Preview{}, err
	}
	if revision != "" {
		if !objectID(revision) {
			return Preview{}, ErrPath
		}
		result, err := r.git(ctx, "show", "--format=", "--no-ext-diff", "--no-textconv", revision, "--", ".")
		return preview(result, err, ErrPath)
	}
	if !validPath(name) {
		return Preview{}, ErrPath
	}
	result, err := r.git(ctx, "diff", "--no-ext-diff", "--no-textconv", "HEAD", "--", name)
	if err == nil && result.ExitCode != 0 {
		result, err = r.git(ctx, "diff", "--no-ext-diff", "--no-textconv", "--cached", "--", name)
	}
	if err == nil && result.ExitCode == 0 && result.Stdout == "" {
		return r.file(ctx, r.scoped(name))
	}
	return preview(result, err, ErrPath)
}

func (r Repository) Commit(ctx context.Context, email, message string, selected []string) (_ string, err error) {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 4096 || strings.ContainsRune(message, 0) || len(selected) == 0 {
		return "", ErrCommit
	}
	changes, err := r.Changes(ctx)
	if err != nil {
		return "", err
	}
	if len(selected) > len(changes.Entries) {
		return "", ErrCommit
	}
	byPath := make(map[string]Change, len(changes.Entries))
	for _, change := range changes.Entries {
		byPath[change.Path] = change
	}
	var untracked, paths []string
	for _, name := range selected {
		entry, ok := byPath[name]
		if !ok {
			return "", ErrCommit
		}
		delete(byPath, name)
		if entry.Untracked {
			untracked = append(untracked, name)
		}
		paths = append(paths, name)
		if entry.Original != "" {
			paths = append(paths, entry.Original)
		}
	}
	if len(untracked) > 0 {
		defer func() {
			if err == nil {
				return
			}
			cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
			defer stop()
			_, resetErr := r.git(cleanup, append([]string{"reset", "--"}, untracked...)...)
			err = errors.Join(err, resetErr)
		}()
		staged, stageErr := r.git(ctx, append([]string{"add", "-N", "--"}, untracked...)...)
		if stageErr != nil || staged.ExitCode != 0 {
			return "", errors.Join(ErrCommit, stageErr)
		}
	}
	name, _, _ := strings.Cut(email, "@")
	args := []string{"-c", "user.name=" + name, "-c", "user.email=" + email,
		"commit", "--only", "--message", message, "--"}
	committed, err := r.git(ctx, append(args, paths...)...)
	if err != nil || committed.ExitCode != 0 {
		return "", errors.Join(ErrCommit, err)
	}
	head, err := r.git(ctx, "rev-parse", "--short", "HEAD")
	if err != nil || head.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(head.Stdout), nil
}

func validPath(name string) bool {
	return name != "." && len(name) <= 1024 && fs.ValidPath(name) && !strings.ContainsRune(name, 0) &&
		!slices.Contains(strings.Split(name, "/"), ".git")
}

func objectID(value string) bool {
	return (len(value) == 40 || len(value) == 64) && strings.Trim(value, "0123456789abcdef") == ""
}

func terminatedFields(text string) []string {
	fields := strings.Split(text, "\x00")
	return fields[:len(fields)-1]
}

func preview(result sandbox.Result, err, unavailable error) (Preview, error) {
	if err != nil {
		return Preview{}, err
	}
	if result.ExitCode != 0 {
		return Preview{}, unavailable
	}
	if strings.ContainsRune(result.Stdout, 0) {
		return Preview{}, ErrBinary
	}
	text := result.Stdout
	if result.Truncated {
		text = strings.ToValidUTF8(text, "�")
	}
	if !utf8.ValidString(text) {
		return Preview{}, fmt.Errorf("%w: invalid UTF-8", ErrBinary)
	}
	return Preview{Text: text, Truncated: result.Truncated}, nil
}
