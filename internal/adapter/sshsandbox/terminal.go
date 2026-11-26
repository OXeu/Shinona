package sshsandbox

import (
	"context"
	"errors"
	"io"
	"net"

	"golang.org/x/crypto/ssh"

	"github.com/OXeu/shinona/internal/sandbox"
)

type terminal struct {
	io.Reader
	input   io.WriteCloser
	session *ssh.Session
	stop    func() bool
}

func (w *workspace) OpenTerminal(ctx context.Context, size sandbox.TerminalSize) (_ sandbox.Terminal, err error) {
	w.inUse.RLock()
	defer w.inUse.RUnlock()
	if w.closed.Load() {
		return nil, net.ErrClosed
	}
	if !size.Valid() {
		return nil, errors.New("invalid terminal size")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	command, err := dockerCommand([]string{"exec", "-it", w.name, "/bin/bash", "-i"})
	if err != nil {
		return nil, err
	}
	// A terminal can own a channel until the workspace closes.
	w.discard.Store(true)
	session, err := w.client.NewSession()
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = session.Close() })
	defer func() {
		if err != nil {
			stop()
			_ = session.Close()
		}
	}()
	if err := session.RequestPty("xterm-256color", int(size.Rows), int(size.Cols), ssh.TerminalModes{}); err != nil {
		return nil, err
	}
	output, err := session.StdoutPipe()
	if err != nil {
		return nil, err
	}
	input, err := session.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, err
	}
	return &terminal{Reader: output, input: input, session: session, stop: stop}, nil
}

func (t *terminal) Input(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, err := t.input.Write(data)
	return err
}

func (t *terminal) Resize(ctx context.Context, size sandbox.TerminalSize) error {
	if !size.Valid() {
		return errors.New("invalid terminal size")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return t.session.WindowChange(int(size.Rows), int(size.Cols))
}

func (t *terminal) Close(context.Context) error {
	t.stop()
	return t.session.Close()
}

var _ sandbox.TerminalWorkspace = (*workspace)(nil)
