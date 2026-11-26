package run

import (
	"context"
	"errors"
	"io/fs"
	"slices"
	"strings"
)

// ErrWorkspaceRoot reports an unusable workspace directory selection.
var ErrWorkspaceRoot = errors.New("select a folder inside the workspace")

// maxWorkspaceRoot bounds the stored directory so the preference stays small.
const maxWorkspaceRoot = 1024

// WorkspaceRoot returns the directory a session uses for workspace changes and
// history. An empty result selects the project workspace root.
func (s *Service) WorkspaceRoot(ctx context.Context, owner, sessionID string) (string, error) {
	return s.repo.WorkspaceRoot(ctx, owner, sessionID)
}

// SetWorkspaceRoot records the directory that changes, history, and commits use
// for a session. An empty directory restores the project workspace root.
func (s *Service) SetWorkspaceRoot(ctx context.Context, owner, sessionID, root string) (string, error) {
	root = strings.TrimSpace(root)
	if root == "." {
		root = ""
	}
	if root != "" && !validWorkspaceRoot(root) {
		return "", ErrWorkspaceRoot
	}
	if err := s.repo.SetWorkspaceRoot(ctx, owner, sessionID, root); err != nil {
		return "", err
	}
	return root, nil
}

func validWorkspaceRoot(root string) bool {
	return len(root) <= maxWorkspaceRoot && fs.ValidPath(root) && !strings.ContainsRune(root, 0) &&
		!slices.Contains(strings.Split(root, "/"), ".git")
}
