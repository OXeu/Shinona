// Package sandbox defines the operations an agent can perform in its workspace.
package sandbox

import (
	"context"
	"errors"
	"time"
)

type Policy string

const (
	Fresh  Policy = "FRESH"
	Shared Policy = "SHARED"
)

var (
	ErrPolicy           = errors.New("that sandbox policy is unavailable for this account")
	ErrWorkspaceMissing = errors.New("this project has no retained workspace")
)

// Spec contains provider-independent information about a requested lease.
type Spec struct {
	RunID     string
	OwnerID   string
	SessionID string
	ProjectID string
	Policy    Policy
	TTL       time.Duration
	Template  string
	Timeout   time.Duration
	OnTimeout string
}

// Provider acquires a workspace for a run. It owns cleanup if acquisition fails.
type Provider interface {
	// Policies returns the supported choices for this owner in default order.
	Policies(ownerID string) []Policy
	Acquire(ctx context.Context, spec Spec) (Workspace, error)
}

// RetainedProvider can reopen the workspace of a finished run for inspection.
type RetainedProvider interface {
	Provider
	OpenRetained(ctx context.Context, spec Spec) (Workspace, error)
}

// ProjectProvider shares a workspace across an owner's sessions in a project.
// An empty ProjectID selects the owner's default project. OpenProject must
// return ErrWorkspaceMissing when no workspace has been provisioned.
type ProjectProvider interface {
	Provider
	OpenProject(ctx context.Context, spec Spec) (Workspace, error)
}

type ProjectState string

const (
	ProjectNone        ProjectState = "NONE"
	ProjectRunning     ProjectState = "RUNNING"
	ProjectPaused      ProjectState = "PAUSED"
	ProjectStopped     ProjectState = "STOPPED"
	ProjectMissing     ProjectState = "MISSING"
	ProjectUnavailable ProjectState = "UNAVAILABLE"
)

type ProjectStatus struct {
	ID    string
	State ProjectState
}

type ManagedProjectProvider interface {
	ProjectProvider
	ProjectStatus(context.Context, Spec) (ProjectStatus, error)
	DestroyProject(context.Context, Spec) error
}

const (
	TimeoutPause      = "pause"
	TimeoutKill       = "kill"
	MaxTimeoutSeconds = 86400
)

// Workspace owns the lease until Close. Paths are relative to its working
// directory. A workspace directory is a convenience, not an isolation boundary.
// Inspection commands can run concurrently with the agent's operations.
// Close releases the provider's resources and must accept a fresh context.
type Workspace interface {
	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	Exec(ctx context.Context, command Command) (Result, error)
	Close(ctx context.Context) error
}

// Command invokes a program inside the workspace. Shell use is explicit.
type Command struct {
	Program string
	Args    []string
}

// Result contains process output. A successful Exec includes the exit code,
// even for a nonzero exit. On error, Exec may return output captured so far.
type Result struct {
	ExitCode   int
	ExitDetail string
	Stdout     string
	Stderr     string
	Truncated  bool
}
