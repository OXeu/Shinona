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

const maxRecoveryPages = 64

func (p *Provider) recoverProject(ctx context.Context, spec sandbox.Spec, previous projectSandbox) (*projectWorkspace, error) {
	pages := p.api.ListSnapshots(ctx, sdk.SnapshotListOptions{SandboxID: previous.id})
	attempted := make(map[string]bool)
	visited := make(map[[sha256.Size]byte]struct{})
	for page := 0; pages.HasNext(); page++ {
		if page == maxRecoveryPages {
			return nil, fmt.Errorf("E2B snapshot listing exceeds the %d-page limit", maxRecoveryPages)
		}
		cursor := sha256.Sum256([]byte(pages.NextToken()))
		if _, repeated := visited[cursor]; repeated {
			return nil, errors.New("E2B returned a repeated snapshot page cursor")
		}
		visited[cursor] = struct{}{}
		request, cancel := context.WithTimeout(ctx, apiTimeout)
		snapshots, err := pages.NextItems(request)
		cancel()
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
	status := httpStatus(err)
	for _, expected := range statuses {
		if status == expected {
			return true
		}
	}
	return false
}

func httpStatus(err error) int {
	failure, ok := errors.AsType[interface {
		error
		HTTPStatus() int
	}](err)
	if ok {
		return failure.HTTPStatus()
	}
	return 0
}
