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
	input     io.WriteCloser
	session   *ssh.Session
	workspace *workspace
	stop      func() bool
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
	// A terminal can own a channel until the workspace closes.
	w.discard.Store(true)
	setup, cancelSetup := context.WithTimeout(ctx, connectTimeout)
	defer cancelSetup()
	stopSetup := context.AfterFunc(setup, w.abortTransport)
	defer stopSetup()
	defer func() {
		if cause := setup.Err(); err != nil && cause != nil {
			err = cause
		}
	}()
	session, err := w.openSession(setup)
	if err != nil {
		return nil, err
	}
	terminal := &terminal{session: session, workspace: w}
	terminal.stop = context.AfterFunc(ctx, func() { terminal.interrupt(ctx) })
	defer func() {
		if err != nil {
			_ = terminal.Close(context.WithoutCancel(ctx))
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
	guard, err := w.startTerminalGuard(setup)
	if err != nil {
		return nil, err
	}
	shell := sandbox.Shell("-i")
	args := []string{"exec", "-it", w.name, "sh", "-c", terminalCommand, "sh", guard[0], guard[1], shell.Program}
	command, err := dockerCommand(append(args, shell.Args...))
	if err != nil {
		return nil, err
	}
	if err := session.Start(command); err != nil {
		return nil, err
	}
	stopSetup()
	if err := setup.Err(); err != nil {
		return nil, err
	}
	terminal.Reader, terminal.input = output, input
	return terminal, nil
}

func (t *terminal) Input(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { t.interrupt(ctx) })
	defer stop()
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
	stop := context.AfterFunc(ctx, func() { t.interrupt(ctx) })
	defer stop()
	return t.session.WindowChange(int(size.Rows), int(size.Cols))
}

func (t *terminal) Close(ctx context.Context) error {
	t.stop()
	defer t.workspace.abortTransport()
	return t.workspace.closeSession(ctx, t.session)
}

func (t *terminal) interrupt(ctx context.Context) {
	// The terminal owns this transport. Closing it also wakes a reader
	// when the peer receives the channel close but never acknowledges it.
	defer t.workspace.abortTransport()
	_ = t.workspace.closeSession(context.WithoutCancel(ctx), t.session)
}

var _ sandbox.TerminalWorkspace = (*workspace)(nil)
