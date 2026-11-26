package sshsandbox

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

type workspace struct {
	client  *ssh.Client
	name    string
	cleanup time.Duration
	release func() error
	inUse   sync.RWMutex
	closed  atomic.Bool
	discard atomic.Bool
	shared  bool
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

type dockerStatus struct {
	sandbox.ProjectStatus
	created, hardened bool
}

func (w *workspace) status(ctx context.Context) (sandbox.ProjectStatus, error) {
	status, err := w.containerStatus(ctx)
	return status.ProjectStatus, err
}

func (w *workspace) containerStatus(ctx context.Context) (dockerStatus, error) {
	defer timing.Start(ctx, "ssh_project_status")()
	const format = `{{.ID}} {{.State}} {{if eq (.Label "` + containerPolicyLabel + `") "` + containerPolicy + `"}}hardened{{else}}legacy{{end}}`
	result, err := w.command(ctx, nil, sandbox.DefaultOutputLimit,
		"ps", "--all", "--no-trunc", "--filter", "name=^/"+w.name+"$", "--format", format)
	if err = commandError(result, err); err != nil {
		return dockerStatus{}, err
	}
	fields := strings.Fields(result.Stdout)
	if len(fields) == 0 {
		return dockerStatus{ProjectStatus: sandbox.ProjectStatus{State: sandbox.ProjectNone}}, nil
	}
	if len(fields) != 3 {
		return dockerStatus{}, errors.New("unexpected project sandbox status from Docker")
	}
	status := dockerStatus{ProjectStatus: sandbox.ProjectStatus{ID: fields[0]}, created: fields[1] == "created", hardened: fields[2] == "hardened"}
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
	result, err := w.command(ctx, nil, sandbox.DefaultOutputLimit, args...)
	return commandError(result, err)
}

func commandError(result sandbox.Result, err error) error {
	if err != nil || result.ExitCode == 0 {
		return err
	}
	return fmt.Errorf("SSH sandbox command exited with status %d: %s", result.ExitCode, strings.TrimSpace(result.Stderr))
}

// setsid gives each command its own process group. The Docker stdin pipe is
// reserved for its watchdog so cancellation and disconnects both send EOF.
const runCommand = `
group=$$
exec 3<&0
(
  IFS= read -r ignored <&3 || :
  kill -s KILL -- "-$group" 2>/dev/null
) &
watchdog=$!
"$@" </dev/null 3<&-
status=$?
kill "$watchdog" 2>/dev/null || :
wait "$watchdog" 2>/dev/null || :
exit "$status"`

// Launch setsid from a child of the Docker process group leader. It can
// create a session without forking, and the parent returns the exit status.
const startCommand = `
exec 3<&0
setsid "$@" <&3 3<&- &
wait "$!"`

func (w *workspace) Exec(ctx context.Context, command sandbox.Command) (sandbox.Result, error) {
	args := []string{"exec", "-i", w.name, "sh", "-c", startCommand, "sh", "sh", "-c", runCommand, "sh", command.Program}
	return w.runDocker(ctx, nil, command.OutputLimit(), true, append(args, command.Args...)...)
}

func (w *workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
	clean, err := sandbox.WorkspacePath(name)
	if err != nil {
		return nil, err
	}
	const limit = sandbox.MaxFileBytes + 1
	result, err := w.command(ctx, nil, limit, "exec", w.name,
		"sh", "-c", `test -f "$1" && dd if="$1" bs="$2" count=1 2>/dev/null`, "sh", clean, strconv.Itoa(limit))
	if err = commandError(result, err); err != nil {
		return nil, err
	}
	if len(result.Stdout) > sandbox.MaxFileBytes {
		return nil, errors.New("file exceeds 64 KiB")
	}
	return []byte(result.Stdout), nil
}

func (w *workspace) WriteFile(ctx context.Context, name string, data []byte) error {
	if len(data) > sandbox.MaxFileBytes {
		return errors.New("file exceeds 64 KiB")
	}
	clean, err := sandbox.WorkspacePath(name)
	if err != nil {
		return err
	}
	result, err := w.command(ctx, bytes.NewReader(data), sandbox.DefaultOutputLimit, "exec", "-i", w.name,
		"sh", "-c", `file=./$1; mkdir -p "${file%/*}" && cat > "$file"`, "sh", clean)
	return commandError(result, err)
}

// OpenSSH passes this command through the account's login shell. The bootstrap
// decodes arguments only after entering sh, without reading Docker's stdin.
const dockerBootstrap = `exec sh -c 'for wire do case "$wire" in ` +
	`+*) value=$(printf %s "${wire#+}" | base64 -d && printf .) || exit 2; set -- "$@" "${value%.}";; ` +
	`*) set -- "$@" "$wire";; esac; shift; done; exec docker "$@"' sh`

func dockerCommand(args []string) (string, error) {
	var arguments strings.Builder
	encoded := false
	for _, arg := range args {
		if strings.ContainsRune(arg, 0) {
			return "", errors.New("SSH command arguments cannot contain a null character")
		}
		// A leading '+' marks encoded data. Quotes, backslashes, history
		// expansion, and control bytes vary across login shells.
		if strings.HasPrefix(arg, "+") || strings.IndexFunc(arg, func(ch rune) bool {
			return ch < ' ' || ch == 0x7f || ch == '\'' || ch == '\\' || ch == '!'
		}) >= 0 {
			arguments.WriteString(" +")
			arguments.WriteString(base64.StdEncoding.EncodeToString([]byte(arg)))
			encoded = true
		} else {
			arguments.WriteString(" '")
			arguments.WriteString(arg)
			arguments.WriteByte('\'')
		}
	}
	if encoded {
		return dockerBootstrap + arguments.String(), nil
	}
	return "exec docker" + arguments.String(), nil
}

func (w *workspace) command(ctx context.Context, input io.Reader, limit int, args ...string) (sandbox.Result, error) {
	return w.runDocker(ctx, input, limit, false, args...)
}

func (w *workspace) runDocker(ctx context.Context, input io.Reader, limit int, cancelWithEOF bool, args ...string) (sandbox.Result, error) {
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
	done := make(chan struct{})
	defer close(done)
	defer func() { _ = w.closeSession(context.WithoutCancel(ctx), session) }()
	if err := ctx.Err(); err != nil {
		w.discard.Store(true)
		return sandbox.Result{}, err
	}
	var control io.WriteCloser
	if cancelWithEOF {
		control, err = session.StdinPipe()
		if err != nil {
			return sandbox.Result{}, err
		}
	}
	stop := context.AfterFunc(ctx, func() {
		// Let the channel or the Docker watchdog end the command first. A
		// stalled host may block both the close request and the exit response.
		deadline := time.AfterFunc(w.cleanup, w.abortTransport)
		defer deadline.Stop()
		if control == nil {
			_ = session.Close()
		} else {
			_ = control.Close()
		}
		<-done
	})
	defer stop()
	out, stderr := sandbox.NewOutput(limit), sandbox.NewOutput(sandbox.DefaultOutputLimit)
	session.Stdin, session.Stdout, session.Stderr = input, out, stderr
	err = session.Run(command)
	result := sandbox.Result{Stdout: out.String(), Stderr: stderr.String(), Truncated: out.Truncated() || stderr.Truncated()}
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
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limit := time.AfterFunc(connectTimeout, w.abortTransport)
	defer limit.Stop()
	opened := make(chan struct{})
	defer close(opened)
	stop := context.AfterFunc(ctx, func() {
		if !w.shared {
			w.abortTransport()
			return
		}
		// A live reader shares the agent's transport. Wait briefly for the
		// channel to open so a responsive connection can continue serving it.
		grace := time.NewTimer(w.cleanup)
		defer grace.Stop()
		select {
		case <-opened:
		case <-grace.C:
			w.abortTransport()
		}
	})
	defer stop()
	session, err := w.client.NewSession()
	if err != nil && ctx.Err() != nil {
		return nil, ctx.Err()
	}
	return session, err
}

func (w *workspace) abortTransport() {
	w.discard.Store(true)
	_ = w.Close(context.Background())
}

func (w *workspace) closeSession(ctx context.Context, session *ssh.Session) error {
	cleanup, cancel := context.WithTimeout(ctx, w.cleanup)
	defer cancel()
	stop := context.AfterFunc(cleanup, w.abortTransport)
	defer stop()
	err := session.Close()
	if cleanup.Err() != nil {
		return cleanup.Err()
	}
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
		return nil
	}
	return err
}

var _ sandbox.Workspace = (*workspace)(nil)
