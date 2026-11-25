// Package demo lets contributors exercise the application without external services.
package demo

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
)

type Agent struct{}

func (Agent) Run(ctx context.Context, input run.Input, _ sandbox.Workspace) (run.Result, error) {
	if err := ctx.Err(); err != nil {
		return run.Result{}, err
	}
	parts := []string{input.Prompt}
	for steers := input.Steering.Finish(); len(steers) > 0; steers = input.Steering.Finish() {
		parts = append(parts, steers...)
	}
	for _, image := range input.Payload.Images {
		parts = append(parts, image.Name)
	}
	return run.Result{Output: fmt.Sprintf("Demo response · %d earlier turns in context\n\n%s",
		len(input.History), strings.Join(parts, "\n"))}, nil
}

type Provider struct{}

func (Provider) Policies(string) []sandbox.Policy {
	return []sandbox.Policy{sandbox.Fresh, sandbox.Shared}
}

func (Provider) Acquire(_ context.Context, _ sandbox.Spec) (sandbox.Workspace, error) {
	return workspace{}, nil
}

var errDisabled = errors.New("sandbox operations are disabled in demo mode")

type workspace struct{}

func (workspace) ReadFile(context.Context, string) ([]byte, error) {
	return nil, errDisabled
}

func (workspace) WriteFile(context.Context, string, []byte) error {
	return errDisabled
}

func (workspace) Exec(context.Context, sandbox.Command) (sandbox.Result, error) {
	return sandbox.Result{}, errDisabled
}

func (workspace) Close(context.Context) error {
	return nil
}
