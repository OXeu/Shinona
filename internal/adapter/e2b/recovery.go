package e2b

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"

	sdk "github.com/eric642/e2b-go-sdk"

	"github.com/OXeu/shinona/internal/sandbox"
)

var errSnapshotMissing = errors.New("E2B snapshot no longer exists")

func (p *Provider) recoverProject(ctx context.Context, spec sandbox.Spec, previous projectSandbox) (*projectWorkspace, error) {
	pages := p.api.ListSnapshots(ctx, sdk.SnapshotListOptions{SandboxID: previous.id})
	attempted := make(map[string]bool)
	for pages.HasNext() {
		cursor := pages.NextToken()
		snapshots, err := pages.NextItems(ctx)
		if err != nil {
			if snapshotListUnavailable(err) {
				break
			}
			return nil, fmt.Errorf("list E2B snapshots for the missing sandbox: %w", err)
		}
		for _, snapshot := range snapshots {
			id := snapshot.SnapshotID
			if id == "" || attempted[id] {
				continue
			}
			attempted[id] = true
			workspace, err := p.createProject(ctx, spec, previous, id)
			if !errors.Is(err, errSnapshotMissing) {
				return workspace, err
			}
		}
		if pages.HasNext() && cursor == pages.NextToken() {
			return nil, errors.New("E2B returned the same snapshot page cursor twice")
		}
	}
	if id := previous.tokens.RecoverySnapshot; id != "" && !attempted[id] {
		workspace, err := p.createProject(ctx, spec, previous, id)
		if !errors.Is(err, errSnapshotMissing) {
			return workspace, err
		}
	}
	return p.createProject(ctx, spec, previous, "")
}

func snapshotKey(runID, snapshot string) string {
	if runID == "" {
		return ""
	}
	return fmt.Sprintf("restore-%x", sha256.Sum256([]byte(runID+"\x00"+snapshot)))
}

func isGone(err error) bool {
	return hasStatus(err, http.StatusNotFound, http.StatusGone)
}

func snapshotListUnavailable(err error) bool {
	return hasStatus(err, http.StatusNotFound, http.StatusGone, http.StatusMethodNotAllowed, http.StatusNotImplemented)
}

func hasStatus(err error, statuses ...int) bool {
	failure, ok := errors.AsType[interface {
		error
		HTTPStatus() int
	}](err)
	if !ok {
		return false
	}
	for _, status := range statuses {
		if failure.HTTPStatus() == status {
			return true
		}
	}
	return false
}
