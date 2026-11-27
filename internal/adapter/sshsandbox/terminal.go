package sshsandbox

import (
	"context"
	"errors"
	"io"
	"net"

	"golang.org/x/crypto/ssh"
	"golang.org/x/sync/errgroup"

	"github.com/OXeu/shinona/internal/sandbox"
)

type terminal struct {
	io.Reader
	input     io.WriteCloser
	session   *ssh.Session
	guard     terminalGuard
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
	deadline, cancelDeadline := context.WithTimeout(ctx, connectTimeout)
	defer cancelDeadline()
	setup, cancelSetup := context.WithCancelCause(deadline)
	defer cancelSetup(nil)
	stopSetup := context.AfterFunc(setup, w.abortTransport)
	defer stopSetup()
	defer func() {
		if cause := context.Cause(setup); err != nil && cause != nil {
			err = cause
		}
	}()
	terminal := &terminal{workspace: w}
	defer func() {
		if err != nil {
			_ = terminal.close(context.WithoutCancel(ctx))
		}
	}()
	// The guard must be alive before starting the shell. SSH can negotiate
	// the PTY on its own channel while the guard starts.
	var startup errgroup.Group
	startup.Go(func() error {
		guard, err := w.startTerminalGuard(setup)
		terminal.guard = guard
		if err != nil {
			cancelSetup(err)
		}
		return err
	})
	startup.Go(func() error {
		err := terminal.preparePTY(setup, size)
		if err != nil {
			cancelSetup(err)
		}
		return err
	})
	if err := startup.Wait(); err != nil {
		return nil, err
	}
	shell := sandbox.Shell("-i")
	guard := terminal.guard
	args := []string{"exec", "-it", w.name, "sh", "-c", terminalCommand, "sh", guard.pid, guard.started, shell.Program}
	command, err := dockerCommand(append(args, shell.Args...))
	if err != nil {
		return nil, err
	}
	if err := terminal.session.Start(command); err != nil {
		return nil, err
	}
	stopSetup()
	if err := context.Cause(setup); err != nil {
		return nil, err
	}
	terminal.stop = context.AfterFunc(ctx, func() { _ = terminal.close(context.WithoutCancel(ctx)) })
	return terminal, nil
}

func (t *terminal) preparePTY(ctx context.Context, size sandbox.TerminalSize) error {
	session, err := t.workspace.openSession(ctx)
	if err != nil {
		return err
	}
	t.session = session
	if err := session.RequestPty("xterm-256color", int(size.Rows), int(size.Cols), ssh.TerminalModes{}); err != nil {
		return err
	}
	t.Reader, err = session.StdoutPipe()
	if err != nil {
		return err
	}
	t.input, err = session.StdinPipe()
	return err
}

func (t *terminal) Input(ctx context.Context, data []byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = t.close(context.WithoutCancel(ctx)) })
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
	stop := context.AfterFunc(ctx, func() { _ = t.close(context.WithoutCancel(ctx)) })
	defer stop()
	return t.session.WindowChange(int(size.Rows), int(size.Cols))
}

func (t *terminal) Close(ctx context.Context) error {
	t.stop()
	return t.close(ctx)
}

func (t *terminal) close(ctx context.Context) error {
	// The terminal owns this transport. Closing it also wakes a reader
	// when the peer receives the channel close but never acknowledges it.
	defer t.workspace.abortTransport()
	var err error
	if t.guard.session != nil {
		err = t.guard.close(ctx, t.workspace)
	}
	if t.session != nil {
		err = errors.Join(err, t.workspace.closeSession(ctx, t.session))
	}
	return err
}

var _ sandbox.TerminalWorkspace = (*workspace)(nil)
