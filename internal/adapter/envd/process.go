package envd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"

	"github.com/OXeu/shinona/internal/sandbox"
)

const maxOutputBytes = 16 * 1024

type processConfig struct {
	Command string            `json:"cmd"`
	Args    []string          `json:"args,omitempty"`
	CWD     string            `json:"cwd"`
	Envs    map[string]string `json:"envs,omitempty"`
}

type startRequest struct {
	Process processConfig `json:"process"`
	Tag     string        `json:"tag"`
	Stdin   bool          `json:"stdin"`
	PTY     *terminalPTY  `json:"pty,omitempty"`
}

type startResponse struct {
	Event struct {
		Start *struct {
			PID uint32 `json:"pid"`
		} `json:"start"`
		Data *struct {
			Stdout []byte `json:"stdout"`
			Stderr []byte `json:"stderr"`
			PTY    []byte `json:"pty"`
		} `json:"data"`
		End *struct {
			ExitCode int    `json:"exitCode"`
			Error    string `json:"error"`
		} `json:"end"`
	} `json:"event"`
}

type signalRequest struct {
	Process struct {
		Tag string `json:"tag"`
	} `json:"process"`
	Signal int `json:"signal"`
}

// jsonCodec lets Connect frame the narrow subset of envd RPCs used here.
type jsonCodec struct{}

func (jsonCodec) Name() string                           { return "json" }
func (jsonCodec) Marshal(value any) ([]byte, error)      { return json.Marshal(value) }
func (jsonCodec) Unmarshal(data []byte, value any) error { return json.Unmarshal(data, value) }

func (w *Workspace) Exec(ctx context.Context, command sandbox.Command) (sandbox.Result, error) {
	if err := ctx.Err(); err != nil {
		return sandbox.Result{}, err
	}
	tag := uuid.NewString()
	result, err := w.streamProcess(ctx, &startRequest{
		Process: processConfig{Command: command.Program, Args: command.Args, CWD: w.config.Directory},
		Tag:     tag,
	})
	if err == nil {
		return result, nil
	}
	cleanupCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 3*time.Second)
	defer stop()
	if stopErr := w.stopProcess(cleanupCtx, tag); stopErr != nil {
		err = errors.Join(err, fmt.Errorf("could not confirm that envd stopped the command: %w", stopErr))
	}
	return result, err
}

func (w *Workspace) streamProcess(ctx context.Context, input *startRequest) (result sandbox.Result, err error) {
	client := connect.NewClient[startRequest, startResponse](w.client, w.config.URL+"/process.Process/Start",
		connect.WithCodec(jsonCodec{}), connect.WithReadMaxBytes(1024*1024))
	request := connect.NewRequest(input)
	w.setAuth(request.Header())
	stream, err := client.CallServerStream(ctx, request)
	if err != nil {
		return result, fmt.Errorf("start envd process: %w", err)
	}
	defer func() { _ = stream.Close() }()

	var stdout, stderr []byte
	defer func() { result.Stdout, result.Stderr = string(stdout), string(stderr) }()
	for stream.Receive() {
		event := stream.Msg().Event
		if event.Data != nil {
			stdout = appendOutput(stdout, event.Data.Stdout, &result.Truncated)
			stderr = appendOutput(stderr, event.Data.Stderr, &result.Truncated)
		}
		if event.End != nil {
			if event.End.Error != "" && event.End.ExitCode == 0 {
				return result, fmt.Errorf("envd process failed: %s", event.End.Error)
			}
			result.ExitCode = event.End.ExitCode
			result.ExitDetail = event.End.Error
			return result, nil
		}
	}
	if err := stream.Err(); err != nil {
		return result, fmt.Errorf("read envd process output: %w", err)
	}
	return result, errors.New("envd command stream closed before process exit")
}

func (w *Workspace) stopProcess(ctx context.Context, tag string) error {
	client := connect.NewClient[signalRequest, struct{}](w.client, w.config.URL+"/process.Process/SendSignal",
		connect.WithCodec(jsonCodec{}), connect.WithReadMaxBytes(1024*1024))
	input := signalRequest{Signal: 9}
	input.Process.Tag = tag
	request := connect.NewRequest(&input)
	w.setAuth(request.Header())
	_, err := client.CallUnary(ctx, request)
	if connect.CodeOf(err) == connect.CodeNotFound {
		return nil
	}
	return err
}

func appendOutput(buffer, data []byte, truncated *bool) []byte {
	remaining := maxOutputBytes - len(buffer)
	if len(data) > remaining {
		*truncated = true
		data = data[:remaining]
	}
	return append(buffer, data...)
}
