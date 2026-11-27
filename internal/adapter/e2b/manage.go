package e2b

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	sdk "github.com/eric642/e2b-go-sdk"

	"github.com/OXeu/shinona/internal/sandbox"
)

func (p *Provider) ProjectStatus(ctx context.Context, spec sandbox.Spec) (sandbox.ProjectStatus, error) {
	id, _, err := p.store.E2BSandbox(ctx, spec.OwnerID, spec.ProjectID, p.endpoint)
	status := sandbox.ProjectStatus{ID: id, State: sandbox.ProjectNone}
	if err != nil || id == "" {
		return status, err
	}
	info, err := p.api.GetInfo(ctx, id)
	switch {
	case isGone(err):
		status.State = sandbox.ProjectMissing
		return status, nil
	case err != nil:
		return status, err
	case info.State == sdk.SandboxStateRunning:
		status.State = sandbox.ProjectRunning
	case info.State == sdk.SandboxStatePaused:
		status.State = sandbox.ProjectPaused
	default:
		status.State = sandbox.ProjectUnavailable
	}
	if lifecycle := info.Lifecycle; lifecycle != nil {
		switch lifecycle.OnTimeout {
		case sandbox.TimeoutPause, sandbox.TimeoutKill:
			status.OnTimeout = lifecycle.OnTimeout
		}
	}
	return status, nil
}

func (p *Provider) DestroyProject(ctx context.Context, spec sandbox.Spec, expectedID *string) error {
	key := p.leaseKey(spec)
	release, err := p.leases.gates.Acquire(ctx, key, false)
	if err != nil {
		return err
	}
	defer release()
	id, _, err := p.store.E2BSandbox(ctx, spec.OwnerID, spec.ProjectID, p.endpoint)
	if err != nil {
		return err
	}
	if expectedID != nil && id != *expectedID {
		return sandbox.ErrProjectChanged
	}
	if id == "" {
		p.leases.update(key, "", 0, nil)
		return nil
	}
	if err := p.killSandbox(ctx, id); err != nil {
		return err
	}
	// Give the binding deletion its own budget after remote confirmation.
	p.leases.update(key, "", 0, nil)
	cleanup, stop := context.WithTimeout(context.WithoutCancel(ctx), p.config.CleanupTimeout)
	defer stop()
	return p.store.DeleteE2BSandbox(cleanup, spec.OwnerID, spec.ProjectID, p.endpoint, id)
}

func (p *Provider) killSandbox(ctx context.Context, id string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	_, killErr := p.api.Kill(ctx, id)
	if killErr == nil || isGone(killErr) {
		return nil
	}
	status := httpStatus(killErr)
	if status != 0 && status != http.StatusRequestTimeout && status < http.StatusInternalServerError {
		return killErr
	}
	// The remote deletion may have succeeded before the reply was lost.
	check, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.config.CleanupTimeout)
	defer cancel()
	_, checkErr := p.api.GetInfo(check, id)
	switch {
	case checkErr == nil:
		return killErr
	case isGone(checkErr):
		return nil
	default:
		return errors.Join(killErr, fmt.Errorf("verify E2B sandbox deletion: %w", checkErr))
	}
}
