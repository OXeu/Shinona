package sshsandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// Docker can keep an exec PTY open after its client disconnects. A separate
// non-PTY channel gets EOF on disconnect. Its process needs no cleanup file.
const terminalGuardCommand = `
IFS= read -r stat < "/proc/$$/stat" || exit 1
set -- ${stat##*) }
printf '%s %s\n' "$$" "${20}"
IFS= read -r ignored || :`

const terminalCommand = `
guard_pid=$1 guard_started=$2
shift 2
guard_alive() {
  IFS= read -r stat < "/proc/$guard_pid/stat" || return 1
  set -- ${stat##*) }
  [ "$1" != Z ] && [ "${20}" = "$guard_started" ]
}
guard_alive 2>/dev/null || exit 1
IFS= read -r stat < "/proc/$$/stat" || exit 1
groups=${stat##*) }
kill_jobs() {
  for proc in /proc/[0-9]*/stat; do
    IFS= read -r stat 2>/dev/null < "$proc" || continue
    set -- ${stat##*) }
    if [ "$4" = "$terminal_session" ] && [ "$3" != "$terminal_group" ]; then
      kill -s KILL -- "-$3" 2>/dev/null || :
    fi
  done
}
terminal_groups() {
  set -- $groups
  terminal_group=$3 terminal_session=$4
}
terminal_groups
# Only clean up an independent session created for this Docker exec PTY.
[ "$terminal_group" = "$$" ] && [ "$terminal_session" = "$$" ] || exit 1
(
  while guard_alive; do sleep 1 || break; done
  kill_jobs
  kill -s KILL -- "-$terminal_group"
) </dev/null >/dev/null 2>&1 &
watchdog=$!
"$@"
status=$?
kill "$watchdog" 2>/dev/null || :
wait "$watchdog" 2>/dev/null || :
kill_jobs
exit "$status"`

type terminalGuard struct {
	session *ssh.Session
	stdin   io.WriteCloser
	pid     string
	started string
}

func (g terminalGuard) close(ctx context.Context, w *workspace) error {
	cleanup, cancel := context.WithTimeout(ctx, w.cleanup)
	defer cancel()
	stop := context.AfterFunc(cleanup, w.abortTransport)
	defer stop()
	if g.stdin != nil {
		_ = g.stdin.Close()
	}
	return w.closeSession(cleanup, g.session)
}

func (w *workspace) startTerminalGuard(ctx context.Context) (_ terminalGuard, err error) {
	command, err := dockerCommand([]string{"exec", "-i", w.name, "sh", "-c", terminalGuardCommand})
	if err != nil {
		return terminalGuard{}, err
	}
	session, err := w.openSession(ctx)
	if err != nil {
		return terminalGuard{}, err
	}
	guard := terminalGuard{session: session}
	defer func() {
		if err != nil {
			_ = guard.close(context.WithoutCancel(ctx), w)
		}
	}()
	output, err := session.StdoutPipe()
	if err != nil {
		return terminalGuard{}, err
	}
	guard.stdin, err = session.StdinPipe()
	if err != nil {
		return terminalGuard{}, err
	}
	if err := session.Start(command); err != nil {
		return terminalGuard{}, err
	}
	guard.pid, guard.started, err = readGuardIdentity(output)
	if err != nil {
		return terminalGuard{}, fmt.Errorf("start SSH terminal watchdog: %w", err)
	}
	return guard, nil
}

func readGuardIdentity(output io.Reader) (pid, started string, err error) {
	reader := bufio.NewReader(output)
	var skipping bool
	for {
		line, err := reader.ReadSlice('\n')
		if errors.Is(err, bufio.ErrBufferFull) {
			skipping = true
			continue
		}
		if err != nil {
			return "", "", err
		}
		if skipping {
			skipping = false
			continue
		}
		fields := strings.Fields(string(line))
		if len(fields) == 2 {
			id, idErr := strconv.ParseUint(fields[0], 10, 64)
			start, startErr := strconv.ParseUint(fields[1], 10, 64)
			if idErr == nil && startErr == nil && id > 0 && start > 0 {
				return fields[0], fields[1], nil
			}
		}
	}
}
