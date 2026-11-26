package e2b

import (
	"context"

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
	return status, nil
}

func (p *Provider) DestroyProject(ctx context.Context, spec sandbox.Spec) error {
	release, err := p.provision.Acquire(ctx, spec.ProjectKey(), false)
	if err != nil {
		return err
	}
	defer release()
	id, _, err := p.store.E2BSandbox(ctx, spec.OwnerID, spec.ProjectID, p.endpoint)
	if err != nil || id == "" {
		return err
	}
	if _, err := p.api.Kill(ctx, id); err != nil && !isGone(err) {
		return err
	}
	save, stop := context.WithTimeout(context.WithoutCancel(ctx), apiTimeout)
	defer stop()
	return p.store.DeleteE2BSandbox(save, spec.OwnerID, spec.ProjectID, p.endpoint, id)
}
