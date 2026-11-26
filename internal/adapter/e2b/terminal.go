package e2b

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	sdk "github.com/eric642/e2b-go-sdk"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/sandbox"
)

type terminal struct {
	reader    *io.PipeReader
	cancel    context.CancelFunc
	handle    *sdk.CommandHandle
	workspace *workspace
	tag       string
	closed    sync.Once
	closeErr  error
}

func (w *workspace) OpenTerminal(ctx context.Context, size sandbox.TerminalSize) (sandbox.Terminal, error) {
	if !size.Valid() {
		return nil, errors.New("invalid terminal size")
	}
	stream, cancel := context.WithCancel(w.requestContext(ctx))
	reader, writer := io.Pipe()
	stop := context.AfterFunc(stream, func() { _ = reader.CloseWithError(stream.Err()) })
	shell, tag := sandbox.Shell("-i"), uuid.NewString()
	handle, err := w.box.Pty.Create(stream, sdk.PtyOptions{
		Cmd: shell.Program, Args: shell.Args, User: w.user, Cwd: w.directory, Tag: tag,
		Cols: size.Cols, Rows: size.Rows, RequestTimeout: 10 * time.Second,
		OnData: func(data []byte) { _, _ = writer.Write(data) },
	})
	if err == nil {
		_, err = handle.WaitPID(ctx)
	}
	if err != nil {
		cancel()
		stop()
		_ = reader.Close()
		_ = writer.Close()
		if handle != nil {
			handle.Disconnect()
		}
		return nil, errors.Join(err, w.stopProcess(context.WithoutCancel(ctx), tag))
	}
	go func() {
		defer stop()
		exit, err := handle.Wait(context.WithoutCancel(ctx))
		if exit != nil {
			err = nil
			if exit.Error != "" {
				err = fmt.Errorf("envd terminal: %s", exit.Error)
			}
		}
		_ = writer.CloseWithError(err)
	}()
	return &terminal{reader: reader, cancel: cancel, handle: handle, workspace: w, tag: tag}, nil
}

func (t *terminal) Read(data []byte) (int, error) { return t.reader.Read(data) }

func (t *terminal) Input(ctx context.Context, data []byte) error {
	return t.handle.SendPty(t.workspace.requestContext(ctx), data)
}

func (t *terminal) Resize(ctx context.Context, size sandbox.TerminalSize) error {
	if !size.Valid() {
		return errors.New("invalid terminal size")
	}
	return t.handle.ResizePty(t.workspace.requestContext(ctx), size.Cols, size.Rows)
}

func (t *terminal) Close(ctx context.Context) error {
	t.closed.Do(func() {
		t.cancel()
		_ = t.reader.Close()
		t.handle.Disconnect()
		if _, exited := t.handle.ExitCode(); !exited {
			t.closeErr = t.workspace.stopProcess(ctx, t.tag)
		}
	})
	return t.closeErr
}

var _ sandbox.TerminalWorkspace = (*workspace)(nil)
