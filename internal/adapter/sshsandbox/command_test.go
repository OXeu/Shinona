package sshsandbox

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestContainerStartupUsesIntegerSleepDurations(t *testing.T) {
	dir := t.TempDir()
	const sleep = `#!/bin/sh
case "$1" in ''|*[!0-9]*) exit 2 ;; esac
printf '%s\n' "$1"
[ ! -f slept ] || exit 3
: > slept
`
	if err := os.WriteFile(filepath.Join(dir, "sleep"), []byte(sleep), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := DefaultContainerConfig().createArgs("fixture", "strict-image")
	command := exec.CommandContext(t.Context(), "sh", args[len(args)-2:]...)
	command.Dir = dir
	output, err := command.CombinedOutput()
	if err == nil || len(strings.Fields(string(output))) != 2 {
		t.Fatalf("expected the idle command to wait again and exit if sleep fails: %q, %v", output, err)
	}
}

func TestDockerCommandRoundTripsArgumentsAndInputThroughLoginShell(t *testing.T) {
	dir := t.TempDir()
	const stub = "#!/bin/sh\nprintf '%s\\000' \"$@\"\ncat\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(stub), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("XDG_CONFIG_HOME", dir)
	t.Setenv("XDG_DATA_HOME", dir)
	const input = "stdin stays available to Docker\n"
	for _, shell := range [][]string{{"sh"}, {"dash"}, {"zsh", "-f"}, {"fish", "--no-config"}, {"tcsh", "-f"}} {
		t.Run(shell[0], func(t *testing.T) {
			path, err := exec.LookPath(shell[0])
			if err != nil {
				t.Skipf("%s is unavailable", shell[0])
			}
			for _, args := range [][]string{
				{"ps", "--format", "{{.ID}} {{.State}}"},
				{"exec", "-i", "fixture", "sh", "-c", startCommand, "sh", "sh", "-c", runCommand, "sh", "git", "status"},
				{"exec", "--", "", "+literal", `nested\path\`, "quotes ' and \"", "bang!", "line with final newlines\n\n", `$(exit 99)`, "中文"},
			} {
				command, err := dockerCommand(args)
				if err != nil {
					t.Fatal(err)
				}
				if args[1] == "-i" && !strings.HasPrefix(command, "exec docker ") {
					t.Fatal("ordinary workspace execution used the bootstrap")
				}
				options := append(slices.Clone(shell[1:]), "-c", command)
				runner := exec.CommandContext(t.Context(), path, options...)
				runner.Stdin = strings.NewReader(input)
				output, err := runner.CombinedOutput()
				if err != nil {
					t.Fatalf("Docker command failed: %q, %v", output, err)
				}
				pieces := bytes.Split(output, []byte{0})
				if len(pieces) != len(args)+1 || string(pieces[len(pieces)-1]) != input {
					t.Fatalf("invalid argument count or stdin: %q", output)
				}
				var got []string
				for _, piece := range pieces[:len(pieces)-1] {
					got = append(got, string(piece))
				}
				if !slices.Equal(got, args) {
					t.Fatalf("Docker received %q, want %q", got, args)
				}
			}
		})
	}
	if _, err := dockerCommand([]string{"bad\x00arg"}); err == nil {
		t.Fatal("accepted a null byte in a Docker argument")
	}
}
