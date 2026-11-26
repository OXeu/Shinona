package envd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/sandbox"
)

const terminalStartTimeout = 10 * time.Second

type terminalPTY struct {
	Size sandbox.TerminalSize `json:"size"`
}

type terminalSelector struct {
	Tag string `json:"tag"`
}

type terminalInput struct {
	Process terminalSelector `json:"process"`
	Input   struct {
		PTY []byte `json:"pty"`
	} `json:"input"`
}

type terminalResize struct {
	Process terminalSelector `json:"process"`
	PTY     terminalPTY      `json:"pty"`
}

type terminal struct {
	workspace *Workspace
	stream    *connect.ServerStreamForClient[startResponse]
	cancel    context.CancelFunc
	selector  terminalSelector
	output    []byte
	exited    atomic.Bool
	streamMu  sync.Mutex
	closed    bool
}

func (w *Workspace) OpenTerminal(ctx context.Context, size sandbox.TerminalSize) (sandbox.Terminal, error) {
	if !size.Valid() {
		return nil, errors.New("invalid terminal size")
	}
	streamCtx, cancel := context.WithCancel(ctx)
	timer := time.AfterFunc(terminalStartTimeout, cancel)
	defer timer.Stop()
	client := *w.client
	client.Timeout = 0
	process := connect.NewClient[startRequest, startResponse](&client, w.config.URL+"/process.Process/Start",
		connect.WithCodec(jsonCodec{}), connect.WithReadMaxBytes(1<<20))
	tag := uuid.NewString()
	request := connect.NewRequest(&startRequest{
		Process: processConfig{Command: "/bin/bash", Args: []string{"-i"}, CWD: w.config.Directory,
			Envs: map[string]string{"TERM": "xterm-256color", "LANG": "C.UTF-8"}},
		Tag: tag, Stdin: true, PTY: &terminalPTY{Size: size},
	})
	w.setAuth(request.Header())
	stream, err := process.CallServerStream(streamCtx, request)
	if err == nil {
		if stream.Receive() && stream.Msg().Event.Start != nil && timer.Stop() && streamCtx.Err() == nil {
			return &terminal{workspace: w, stream: stream, cancel: cancel, selector: terminalSelector{Tag: tag}}, nil
		}
		err = stream.Err()
		_ = stream.Close()
	}
	cancel()
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer stop()
	if err == nil {
		err = errors.New("envd did not start the terminal")
	}
	return nil, errors.Join(err, w.stopProcess(cleanup, tag))
}

func (t *terminal) Read(p []byte) (int, error) {
	t.streamMu.Lock()
	defer t.streamMu.Unlock()
	if len(p) == 0 {
		return 0, nil
	}
	if t.closed {
		return 0, io.EOF
	}
	for len(t.output) == 0 {
		if !t.stream.Receive() {
			_ = t.closeStream()
			if err := t.stream.Err(); err != nil {
				return 0, err
			}
			return 0, io.ErrUnexpectedEOF
		}
		event := t.stream.Msg().Event
		if event.End != nil {
			t.exited.Store(true)
			_ = t.closeStream()
			if event.End.Error != "" {
				return 0, fmt.Errorf("envd terminal: %s", event.End.Error)
			}
			return 0, io.EOF
		}
		if event.Data != nil {
			t.output = event.Data.PTY
		}
	}
	n := copy(p, t.output)
	t.output = t.output[n:]
	return n, nil
}

func (t *terminal) Input(ctx context.Context, data []byte) error {
	input := terminalInput{Process: t.selector}
	input.Input.PTY = data
	return terminalCall(ctx, t.workspace, "SendInput", &input)
}

func (t *terminal) Resize(ctx context.Context, size sandbox.TerminalSize) error {
	if !size.Valid() {
		return errors.New("invalid terminal size")
	}
	return terminalCall(ctx, t.workspace, "Update", &terminalResize{Process: t.selector, PTY: terminalPTY{Size: size}})
}

func (t *terminal) Close(ctx context.Context) error {
	t.cancel()
	t.streamMu.Lock()
	streamErr := t.closeStream()
	t.streamMu.Unlock()
	if t.exited.Load() {
		return streamErr
	}
	return errors.Join(streamErr, t.workspace.stopProcess(ctx, t.selector.Tag))
}

func (t *terminal) closeStream() error {
	if t.closed {
		return nil
	}
	t.closed = true
	return t.stream.Close()
}

func terminalCall[T any](ctx context.Context, w *Workspace, method string, data *T) error {
	client := connect.NewClient[T, struct{}](w.client, w.config.URL+"/process.Process/"+method,
		connect.WithCodec(jsonCodec{}), connect.WithReadMaxBytes(1<<20))
	request := connect.NewRequest(data)
	w.setAuth(request.Header())
	_, err := client.CallUnary(ctx, request)
	return err
}

var _ sandbox.TerminalWorkspace = (*Workspace)(nil)
