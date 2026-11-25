package sandbox

import (
	"context"
	"io"
)

const TerminalChunkSize = 16 << 10

type TerminalSize struct {
	Cols uint32 `json:"cols"`
	Rows uint32 `json:"rows"`
}

func (s TerminalSize) Valid() bool {
	return s.Cols >= 2 && s.Cols <= 500 && s.Rows >= 2 && s.Rows <= 300
}

// TerminalWorkspace is the optional interactive shell capability of a workspace.
type TerminalWorkspace interface {
	OpenTerminal(context.Context, TerminalSize) (Terminal, error)
}

// Terminal reads PTY output until the shell exits or the open context is canceled.
// One reader can run concurrently with input, resize, and close operations.
type Terminal interface {
	io.Reader
	Input(context.Context, []byte) error
	Resize(context.Context, TerminalSize) error
	Close(context.Context) error
}
