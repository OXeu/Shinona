// Package workspace reads and commits repository content inside a sandbox lease.
package workspace

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strconv"
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
	Path      string `json:"path"`
	Status    string `json:"status"`
	Untracked bool   `json:"untracked"`
	Original  string `json:"-"`
}

type Changes struct {
	Branch    string   `json:"branch"`
	Entries   []Change `json:"entries"`
	Truncated bool     `json:"truncated"`
}

type Commit struct {
	Hash    string `json:"hash"`
	Short   string `json:"short"`
	Author  string `json:"author"`
	Date    string `json:"date"`
	Subject string `json:"subject"`
}

type History struct {
	Entries   []Commit `json:"entries"`
	Truncated bool     `json:"truncated"`
}

type Entry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Directory bool   `json:"directory"`
}

type Directory struct {
	Entries   []Entry `json:"entries"`
	Truncated bool    `json:"truncated"`
}

type Preview struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
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
	inside, prefix, _ := strings.Cut(result.Stdout, "\n")
	if inside != "true" {
		return "", ErrRepository
	}
	return strings.TrimSuffix(prefix, "\n"), nil
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
	return r.workspace.Exec(ctx, r.gitCommand(0, args...))
}

func (r Repository) gitCommand(limit int, args ...string) sandbox.Command {
	options := []string{"--literal-pathspecs", "-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false",
		"-c", "commit.gpgSign=false", "-c", "color.ui=false"}
	args = append(options, args...)
	command := sandbox.Command{Program: "git", Args: args, StdoutLimit: limit}
	if r.root != "" {
		command.Program = "sh"
		command.Args = append([]string{"-c", resolveDirectory + `shift; exec git "$@"`, "shinona-git", r.root}, args...)
	}
	return command
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
	status, err := r.status(ctx, 0, ".")
	if err != nil {
		return Changes{}, err
	}
	return Changes{
		Branch: strings.TrimSpace(branch.Stdout), Entries: statusEntries(status.Stdout, prefix), Truncated: status.Truncated,
	}, nil
}

func (r Repository) statusCommand(limit int, paths ...string) sandbox.Command {
	// The scope-wide pathspec reports cross-boundary renames as additions
	// or deletions on this side.
	args := []string{"--no-optional-locks", "status", "--porcelain=v1", "-z", "--untracked-files=all", "--"}
	return r.gitCommand(limit, append(args, paths...)...)
}

func (r Repository) status(ctx context.Context, limit int, paths ...string) (sandbox.Result, error) {
	result, err := r.workspace.Exec(ctx, r.statusCommand(limit, paths...))
	if err == nil && result.ExitCode != 0 {
		err = ErrRepository
	}
	return result, err
}

func statusEntries(output, prefix string) []Change {
	entries := []Change{}
	fields := terminatedFields(output)
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
			entries = append(entries, entry)
		}
	}
	return entries
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

const requireWorkspacePath = `case "$target" in "$root"|"${root%/}"/*) ;; *) exit 2 ;; esac; ` +
	`case "$target" in */.git|*/.git/*) exit 2 ;; esac; `

// Directory operations only need the shell's physical directory resolution.
const resolveDirectory = `set -eu; cd -P . || exit 2; root=$PWD; cd -P "./$1" || exit 2; target=$PWD; ` + requireWorkspacePath

// Physical directory resolution cannot follow the final link to a file.
const resolveFile = resolveDirectory + `target=./$2; if [ -L "$target" ]; then ` +
	`target=$(realpath "$target" && printf .) || exit 2; target=${target%??}; ` + requireWorkspacePath + `fi; `

func (r Repository) Directory(ctx context.Context, folder string) (Directory, error) {
	if folder != "." && !validPath(folder) {
		return Directory{}, ErrPath
	}
	result, err := r.workspace.Exec(ctx, sandbox.Command{Program: "sh", Args: []string{
		"-c", resolveDirectory + `for entry in ./* ./.[!.]* ./..?*; do
  if [ "$entry" = "./.git" ] || [ -L "$entry" ]; then continue; fi
  if [ -d "$entry" ]; then
    kind=d
  elif [ -f "$entry" ]; then
    kind=f
  else
    continue
  fi
  printf '%s\000%s\000' "$kind" "${entry#./}"
done`, "shinona-list", folder,
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
	result, err := r.workspace.Exec(ctx, sandbox.Command{Program: "sh", Args: []string{
		"-c", resolveDirectory, "shinona-root", root,
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
	const previewBytes = sandbox.DefaultOutputLimit
	// Read one byte past the preview boundary to distinguish an exact fit.
	result, err := r.workspace.Exec(ctx, sandbox.Command{Program: "sh", StdoutLimit: previewBytes + 1, Args: []string{
		"-c", resolveFile + `[ -f "$target" ] && dd if="$target" bs="$3" count=1 2>/dev/null`,
		"shinona-file", path.Dir(name), path.Base(name), strconv.Itoa(previewBytes + 1),
	}})
	if len(result.Stdout) > previewBytes {
		result.Stdout = result.Stdout[:previewBytes]
		result.Truncated = true
	}
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
	entries, err := r.selectedChanges(ctx, selected)
	if err != nil {
		return "", err
	}
	var untracked, paths []string
	for _, entry := range entries {
		if entry.Untracked {
			untracked = append(untracked, entry.Path)
		}
		paths = append(paths, entry.Path)
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
			reset, resetErr := r.git(cleanup, append([]string{"reset", "--"}, untracked...)...)
			err = errors.Join(err, commitError("roll back intent to add", reset, resetErr))
		}()
		staged, stageErr := r.git(ctx, append([]string{"add", "-N", "--"}, untracked...)...)
		if err := commitError("mark untracked files for addition", staged, stageErr); err != nil {
			return "", err
		}
	}
	name, _, _ := strings.Cut(email, "@")
	args := []string{"-c", "user.name=" + name, "-c", "user.email=" + email,
		"commit", "--only", "--message", message, "--"}
	committed, err := r.git(ctx, append(args, paths...)...)
	if err := commitError("commit selected changes", committed, err); err != nil {
		return "", err
	}
	head, err := r.git(ctx, "rev-parse", "--short", "HEAD")
	if err != nil || head.ExitCode != 0 {
		return "", nil
	}
	return strings.TrimSpace(head.Stdout), nil
}

func (r Repository) selectedChanges(ctx context.Context, selected []string) ([]Change, error) {
	pending := make(map[string]bool, len(selected))
	for _, name := range selected {
		if !validPath(name) || pending[name] {
			return nil, ErrCommit
		}
		pending[name] = true
	}
	prefix, err := r.workTree(ctx)
	if err != nil {
		return nil, err
	}
	status, err := r.status(ctx, sandbox.MaxFileBytes, selected...)
	if err != nil {
		return nil, err
	}
	changes := statusEntries(status.Stdout, prefix)
	// Narrow pathspecs can change how Git pairs renames. Consult the whole
	// scope when a tracked path could be on either side of a move.
	if status.Truncated || slices.ContainsFunc(changes, func(entry Change) bool {
		return pending[entry.Path] && strings.ContainsAny(entry.Status, "ADRC")
	}) {
		status, err = r.status(ctx, sandbox.MaxFileBytes, ".")
		if err != nil {
			return nil, err
		}
		changes = statusEntries(status.Stdout, prefix)
	}
	byPath := make(map[string]Change, len(selected))
	for _, entry := range changes {
		if pending[entry.Path] {
			byPath[entry.Path] = entry
		}
	}
	entries := make([]Change, 0, len(selected))
	for _, name := range selected {
		entry, ok := byPath[name]
		if !ok && status.Truncated {
			entry, err = r.checkTruncatedStatus(ctx, prefix, name)
			if err != nil {
				return nil, err
			}
			ok = true
		}
		if !ok {
			return nil, ErrCommit
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// Keep the scope-wide pathspec to retain rename pairs. Stream the result as
// hex because POSIX awk has no NUL-delimited input mode. An empty record
// signals a successful Git exit; paths and rename sources always have bytes.
const selectStatusEntry = `target=$1; shift
( "$@" && printf '\000' ) | LC_ALL=C od -An -v -tx1 | LC_ALL=C awk -v target="$target" '
{
  for (i = 1; i <= NF; i++) {
    if ($i != "00") {
      record = record $i
      continue
    }
    if (record == "") {
      complete = 1
    } else if (has_source) {
      if (selected) print previous "00" record "00"
      has_source = 0
    } else {
      selected = substr(record, 7) == target
      has_source = substr(record, 1, 4) ~ /^(43|52)|(..)(43|52)$/
      if (has_source) previous = record
      else if (selected) print record "00"
    }
    record = ""
  }
}
END { if (!complete || has_source) exit 2 }'`

func (r Repository) checkTruncatedStatus(ctx context.Context, prefix, name string) (Change, error) {
	command := r.statusCommand(0, ".")
	args := []string{"-c", selectStatusEntry, "shinona-status", hex.EncodeToString([]byte(prefix + name)), command.Program}
	status, err := r.workspace.Exec(ctx, sandbox.Command{
		Program: "sh", Args: append(args, command.Args...), StdoutLimit: sandbox.MaxFileBytes,
	})
	if err != nil {
		return Change{}, err
	}
	if status.ExitCode != 0 {
		if detail := strings.TrimSpace(status.Stderr); detail != "" {
			return Change{}, fmt.Errorf("%w: %s", ErrRepository, detail)
		}
		return Change{}, ErrRepository
	}
	output, err := hex.DecodeString(strings.TrimSpace(status.Stdout))
	if err != nil {
		return Change{}, errors.Join(ErrCommit, err)
	}
	for _, entry := range statusEntries(string(output), prefix) {
		if entry.Path == name {
			return entry, nil
		}
	}
	return Change{}, ErrCommit
}

func commitError(operation string, result sandbox.Result, err error) error {
	if detail := strings.TrimSpace(result.Stderr); detail != "" && (err != nil || result.ExitCode != 0) {
		err = errors.Join(err, errors.New(detail))
	}
	if err != nil {
		return errors.Join(ErrCommit, fmt.Errorf("%s (exit %d): %w", operation, result.ExitCode, err))
	}
	if result.ExitCode != 0 {
		return errors.Join(ErrCommit, fmt.Errorf("%s (exit %d)", operation, result.ExitCode))
	}
	return nil
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
	if !utf8.ValidString(text) {
		for offset := 0; offset < len(text); {
			if result.Truncated && !utf8.FullRuneInString(text[offset:]) {
				text = text[:offset]
				break
			}
			runeValue, size := utf8.DecodeRuneInString(text[offset:])
			if runeValue == utf8.RuneError && size == 1 {
				return Preview{}, fmt.Errorf("%w: invalid UTF-8", ErrBinary)
			}
			offset += size
		}
	}
	return Preview{Text: text, Truncated: result.Truncated}, nil
}
