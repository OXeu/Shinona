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

// WorkspaceSelection pairs a working directory with its project. An empty root
// selects the project workspace root.
type WorkspaceSelection struct {
	ProjectID string
	Root      string
}

func (s *Service) WorkspaceSelection(ctx context.Context, owner, sessionID string) (WorkspaceSelection, error) {
	return s.stores.Projects.WorkspaceSelection(ctx, owner, sessionID)
}

// SetWorkspaceRoot records the directory that changes, history, and commits use
// for the session's project. An empty directory restores the workspace root.
func (s *Service) SetWorkspaceRoot(ctx context.Context, owner, sessionID, root string, validate func(context.Context, string) error) (string, error) {
	if root == "." {
		root = ""
	}
	if root != "" && !validWorkspaceRoot(root) {
		return "", ErrWorkspaceRoot
	}
	projectID, err := s.stores.Sessions.SessionProject(ctx, owner, sessionID)
	if err != nil {
		return "", err
	}
	if root != "" {
		if err := validate(ctx, root); err != nil {
			return "", err
		}
	}
	if err := s.stores.Projects.SetWorkspaceRoot(ctx, owner, sessionID, projectID, root); err != nil {
		return "", err
	}
	return root, nil
}

func validWorkspaceRoot(root string) bool {
	return len(root) <= maxWorkspaceRoot && fs.ValidPath(root) && !strings.ContainsRune(root, 0) &&
		!slices.Contains(strings.Split(root, "/"), ".git")
}
