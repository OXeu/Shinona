package sshsandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

type workspace struct {
	client  *ssh.Client
	name    string
	release func() error
	inUse   sync.RWMutex
	closed  atomic.Bool
	discard atomic.Bool
}

func (w *workspace) Close(context.Context) error {
	if !w.closed.CompareAndSwap(false, true) {
		return nil
	}
	if w.release != nil && w.inUse.TryLock() {
		defer w.inUse.Unlock()
		if !w.discard.Load() {
			return w.release()
		}
	}
	return w.client.Close()
}

func (w *workspace) status(ctx context.Context) (sandbox.ProjectStatus, error) {
	defer timing.Start(ctx, "ssh_project_status")()
	result, err := w.command(ctx, nil, maxOutputBytes,
		"ps", "--all", "--no-trunc", "--filter", "name=^/"+w.name+"$", "--format", "{{.ID}} {{.State}}")
	if err = commandError(result, err); err != nil {
		return sandbox.ProjectStatus{}, err
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) == 0 {
		return sandbox.ProjectStatus{State: sandbox.ProjectNone}, nil
	}
	if len(fields) != 2 {
		return sandbox.ProjectStatus{}, errors.New("unexpected project sandbox status from Docker")
	}
	status := sandbox.ProjectStatus{ID: fields[0]}
	switch fields[1] {
	case "running":
		status.State = sandbox.ProjectRunning
	case "paused":
		status.State = sandbox.ProjectPaused
	default:
		status.State = sandbox.ProjectStopped
	}
	return status, nil
}

func (w *workspace) docker(ctx context.Context, args ...string) error {
	result, err := w.command(ctx, nil, maxOutputBytes, args...)
	return commandError(result, err)
}

func commandError(result sandbox.Result, err error) error {
	if err != nil || result.ExitCode == 0 {
		return err
	}
	return fmt.Errorf("SSH sandbox command exited with status %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
}

// setsid gives each command a process group that can be stopped independently.
const runCommand = `
token=$1
shift
if [ -e "$token.cancel" ]; then rm -f -- "$token.cancel"; exit 125; fi
printf '%s\n' "$$" > "$token" || exit 125
if [ -e "$token.cancel" ]; then rm -f -- "$token" "$token.cancel"; exit 125; fi
"$@"
status=$?
rm -f -- "$token"
exit "$status"`

const stopCommand = `
: > "$1.cancel" || exit 1
if [ -r "$1" ]; then
  read -r pid < "$1" || exit 1
  case "$pid" in ''|*[!0-9]*) exit 1;; esac
  kill -KILL "-$pid" 2>/dev/null || :
  rm -f -- "$1" "$1.cancel"
fi`

func (w *workspace) Exec(ctx context.Context, command sandbox.Command) (sandbox.Result, error) {
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	token := "/run/shinona-" + uuid.NewString()
	args := []string{"exec", w.name, "setsid", "--wait", "/bin/sh", "-c", runCommand, "sh", token, command.Program}
	stopped := make(chan error, 1)
	stop := context.AfterFunc(ctx, func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
		defer cancel()
		stopped <- w.docker(cleanup, "exec", w.name, "/bin/sh", "-c", stopCommand, "sh", token)
	})
	result, err := w.command(ctx, nil, maxOutputBytes, append(args, command.Args...)...)
	if !stop() {
		if stopErr := <-stopped; stopErr != nil {
			return result, errors.Join(err, fmt.Errorf("could not stop the SSH sandbox command: %w", stopErr))
		}
	}
	return result, err
}

func workspacePath(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", errors.New("use a file path relative to the workspace")
	}
	return clean, nil
}

func (w *workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
	clean, err := workspacePath(name)
	if err != nil {
		return nil, err
	}
	result, err := w.command(ctx, nil, maxFileBytes+1, "exec", w.name,
		"/bin/sh", "-c", `test -f "$1" && head -c 65537 -- "$1"`, "sh", clean)
	if err = commandError(result, err); err != nil {
		return nil, err
	}
	if len(result.Stdout) > maxFileBytes {
		return nil, errors.New("file exceeds 64 KiB")
	}
	return []byte(result.Stdout), nil
}

func (w *workspace) WriteFile(ctx context.Context, name string, data []byte) error {
	if len(data) > maxFileBytes {
		return errors.New("file exceeds 64 KiB")
	}
	clean, err := workspacePath(name)
	if err != nil {
		return err
	}
	result, err := w.command(ctx, bytes.NewReader(data), maxOutputBytes, "exec", "-i", w.name,
		"/bin/sh", "-c", `mkdir -p -- "$(dirname -- "$1")" && cat > "$1"`, "sh", clean)
	return commandError(result, err)
}

// The SSH server starts the remote command through a shell. Quote each argument
// before it crosses that boundary, including commands passed to docker exec.
func dockerCommand(args []string) (string, error) {
	var command strings.Builder
	command.WriteString("exec docker")
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return "", errors.New("SSH command arguments cannot contain a null character")
		}
		command.WriteString(" '")
		command.WriteString(strings.ReplaceAll(arg, "'", "'\"'\"'"))
		command.WriteByte('\'')
	}
	return command.String(), nil
}

func (w *workspace) command(ctx context.Context, input io.Reader, limit int, args ...string) (sandbox.Result, error) {
	w.inUse.RLock()
	defer w.inUse.RUnlock()
	if w.closed.Load() {
		return sandbox.Result{}, net.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	command, err := dockerCommand(args)
	if err != nil {
		return sandbox.Result{}, err
	}
	session, err := w.openSession(ctx)
	if err != nil {
		w.discard.Store(true)
		return sandbox.Result{}, err
	}
	defer func() { _ = session.Close() }()
	if err := ctx.Err(); err != nil {
		w.discard.Store(true)
		return sandbox.Result{}, err
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer stop()
	out := limitedOutput{remaining: limit}
	stderr := limitedOutput{remaining: maxOutputBytes}
	session.Stdin, session.Stdout, session.Stderr = input, &out, &stderr
	err = session.Run(command)
	result := sandbox.Result{Stdout: out.buffer.String(), Stderr: stderr.buffer.String(), Truncated: out.truncated || stderr.truncated}
	if ctx.Err() != nil {
		w.discard.Store(true)
		return result, ctx.Err()
	}
	if exit, ok := errors.AsType[*ssh.ExitError](err); ok {
		result.ExitCode, result.ExitDetail = exit.ExitStatus(), exit.Signal()
		return result, nil
	}
	if err != nil {
		w.discard.Store(true)
	}
	return result, err
}

func (w *workspace) openSession(ctx context.Context) (*ssh.Session, error) {
	// A retained transport belongs to this lease alone. Closing it bounds the
	// channel-open wait even if a stale SSH server never acknowledges it.
	if w.release != nil {
		stop := context.AfterFunc(ctx, func() { _ = w.client.Close() })
		defer stop()
	}
	session, err := w.client.NewSession()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return session, err
}

type limitedOutput struct {
	buffer    bytes.Buffer
	remaining int
	truncated bool
}

func (o *limitedOutput) Write(data []byte) (int, error) {
	n := min(o.remaining, len(data))
	o.remaining -= n
	o.truncated = o.truncated || n < len(data)
	_, _ = o.buffer.Write(data[:n])
	return len(data), nil
}

var _ sandbox.Workspace = (*workspace)(nil)
