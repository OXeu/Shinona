package e2b

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path"
	"sync"
	"time"

	sdk "github.com/eric642/e2b-go-sdk"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

type workspace struct {
	box                   *sdk.Sandbox
	route                 envdRoute
	user                  string
	directory             string
	processCleanupTimeout time.Duration
}

func (w *workspace) requestContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, envdRouteKey{}, w.route)
}

func (w *workspace) Close(context.Context) error { return nil }

func (w *workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
	defer timing.Start(ctx, "sandbox_read_file")()
	file, err := sandbox.WorkspacePath(name)
	if err != nil {
		return nil, err
	}
	data, err := w.box.Files.ReadStream(w.requestContext(ctx), path.Join(w.directory, file), sdk.FsOptions{User: w.user})
	if err != nil {
		return nil, err
	}
	defer data.Close()
	body, err := io.ReadAll(io.LimitReader(data, sandbox.MaxFileBytes+1))
	if len(body) > sandbox.MaxFileBytes {
		return nil, errors.New("file exceeds 64 KiB")
	}
	return body, err
}

func (w *workspace) WriteFile(ctx context.Context, name string, data []byte) error {
	if len(data) > sandbox.MaxFileBytes {
		return errors.New("file exceeds 64 KiB")
	}
	file, err := sandbox.WorkspacePath(name)
	if err != nil {
		return err
	}
	_, err = w.box.Files.WriteBytes(w.requestContext(ctx), path.Join(w.directory, file), data, sdk.FsOptions{User: w.user})
	return err
}

func (w *workspace) Exec(ctx context.Context, command sandbox.Command) (sandbox.Result, error) {
	defer timing.Start(ctx, "sandbox_exec")()
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	tag := uuid.NewString()
	var mu sync.Mutex
	stdout, stderr := sandbox.NewOutput(command.OutputLimit()), sandbox.NewOutput(sandbox.DefaultOutputLimit)
	handle, err := w.box.Commands.Start(w.requestContext(ctx), command.Program, sdk.RunOptions{
		Args: command.Args, User: w.user, Cwd: w.directory, Tag: tag, RetainOutput: new(false),
		OnStdout: func(data []byte) {
			mu.Lock()
			defer mu.Unlock()
			_, _ = stdout.Write(data)
		},
		OnStderr: func(data []byte) {
			mu.Lock()
			defer mu.Unlock()
			_, _ = stderr.Write(data)
		},
	})
	var exit *sdk.CommandResult
	if err == nil {
		defer handle.Disconnect()
		exit, err = handle.Wait(ctx)
	}
	if exit == nil && err != nil {
		if stopErr := w.stopProcess(context.WithoutCancel(ctx), tag); stopErr != nil {
			err = errors.Join(err, fmt.Errorf("could not confirm that envd stopped the command: %w", stopErr))
		}
	}
	mu.Lock()
	defer mu.Unlock()
	result := sandbox.Result{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.Truncated() || stderr.Truncated()}
	if exit == nil {
		return result, err
	}
	result.ExitCode, result.ExitDetail = int(exit.ExitCode), exit.Error
	if exit.ExitCode == 0 && exit.Error != "" {
		return result, fmt.Errorf("envd process failed: %s", exit.Error)
	}
	return result, nil
}

func (w *workspace) stopProcess(ctx context.Context, tag string) error {
	cleanup, cancel := context.WithTimeout(ctx, w.processCleanupTimeout)
	defer cancel()
	_, err := w.box.Commands.KillByTag(w.requestContext(cleanup), tag, sdk.CommandRequestOptions{User: w.user})
	return err
}

var _ sandbox.Workspace = (*workspace)(nil)
