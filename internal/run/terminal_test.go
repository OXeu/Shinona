package run

import (
	"bytes"
	"testing"

	"github.com/OXeu/shinona/internal/sandbox"
)

func TestTerminalOutputResumesAtRetainedHistory(t *testing.T) {
	tab := TerminalSession{state: TerminalRunning, changed: make(chan struct{})}
	chunk := bytes.Repeat([]byte{'x'}, sandbox.TerminalChunkSize)
	for range terminalHistorySize / len(chunk) {
		tab.appendOutput(chunk)
	}
	buffer := make([]byte, terminalHistorySize)
	offset, n, state, changed := tab.Output(0, buffer)
	if offset != terminalHistorySize || n != terminalHistorySize || state != TerminalRunning {
		t.Fatalf("initial replay: offset=%d bytes=%d state=%s", offset, n, state)
	}
	tab.appendOutput([]byte("new output"))
	select {
	case <-changed:
	default:
		t.Fatal("attached reader was not notified")
	}
	next, n, _, _ := tab.Output(offset, buffer)
	if string(buffer[:n]) != "new output" || next != offset+uint64(n) {
		t.Fatalf("attached read: offset=%d bytes=%q", next, buffer[:n])
	}
	replayEnd, n, _, _ := tab.Output(0, buffer)
	if replayEnd != next || n != terminalHistorySize || !bytes.HasSuffix(buffer[:n], []byte("new output")) {
		t.Fatalf("replay after wrap: offset=%d bytes=%d", replayEnd, n)
	}
}
