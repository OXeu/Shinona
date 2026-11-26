package sshsandbox

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
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
    stat=$(cat "$proc" 2>/dev/null) || continue
    set -- ${stat##*) }
    if [ "$4" = "$terminal_session" ] && [ "$3" != "$terminal_group" ]; then
      kill -s KILL -- "-$3" 2>/dev/null || :
    fi
  done
}
read -r state parent terminal_group terminal_session rest <<EOF
$groups
EOF
# Only clean up an independent session created for this Docker exec PTY.
[ "$terminal_group" = "$$" ] && [ "$terminal_session" = "$$" ] || exit 1
(
  while guard_alive; do sleep 1; done
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

func (w *workspace) startTerminalGuard(ctx context.Context) ([]string, error) {
	command, err := dockerCommand([]string{"exec", "-i", w.name, "sh", "-c", terminalGuardCommand})
	if err != nil {
		return nil, err
	}
	session, err := w.openSession(ctx)
	if err != nil {
		return nil, err
	}
	output, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	// Keep the write half open until the SSH transport closes.
	if _, err := session.StdinPipe(); err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, err
	}
	go func() { _ = session.Wait() }()
	line, err := bufio.NewReaderSize(output, 64).ReadSlice('\n')
	if err != nil {
		return nil, fmt.Errorf("start SSH terminal watchdog: %w", err)
	}
	fields := strings.Fields(string(line))
	if len(fields) != 2 {
		return nil, errors.New("invalid SSH terminal watchdog identity")
	}
	for _, field := range fields {
		if value, err := strconv.ParseUint(field, 10, 64); err != nil || value == 0 {
			return nil, errors.New("invalid SSH terminal watchdog identity")
		}
	}
	return fields, nil
}
