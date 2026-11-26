package run

import (
	"context"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

type ServiceStores struct {
	Sessions    SessionRepository
	Transcripts TranscriptRepository
	Turns       TurnRepository
	Projects    ProjectRepository
	Preferences PreferenceRepository
}

type SessionRepository interface {
	CreateSession(context.Context, Session) error
	RecentSessions(ctx context.Context, ownerID string, projectID *string, limit int) ([]Session, error)
	SessionListRevision(ctx context.Context, ownerID string) (int64, error)
	sessionProjectReader
	UpdateSession(ctx context.Context, ownerID, sessionID string, changes SessionChanges) (Session, error)
	ArchiveSession(ctx context.Context, ownerID, sessionID string, now time.Time) error
	MarkSessionRead(ctx context.Context, ownerID, sessionID string, through int, throughID string) error
}

type TranscriptRepository interface {
	Transcript(ctx context.Context, ownerID, sessionID string, limit int) (*Transcript, error)
	TranscriptPage(ctx context.Context, ownerID, sessionID string, beforeSequence int) (*Transcript, error)
	StreamTranscript(ctx context.Context, ownerID, sessionID string, write func(TranscriptStream) error) error
	Attachment(ctx context.Context, ownerID, sessionID, turnID string, index int) (*Image, error)
	AttachmentTurnVisible(ctx context.Context, ownerID, sessionID, turnID string) (bool, error)
}

type TurnRepository interface {
	queuedTurnStopper
	// Enqueue checks ownership and the per-session queue bound atomically.
	Enqueue(ctx context.Context, item Run, firstTitle string, queue bool) (Run, error)
	// EnqueueRetry also verifies that the retry target is still the latest failed turn.
	EnqueueRetry(ctx context.Context, item Run, options RetryOptions) (Run, error)
	TakeQueued(ctx context.Context, ownerID, sessionID, id string, textOnly bool, now time.Time) (PendingDraft, error)
	RevertTurn(ctx context.Context, ownerID, sessionID, turnID, expectedHeadID string, now time.Time) (RevertedTurn, error)
}

type ProjectRepository interface {
	ProjectSandboxRepository
	CreateProject(context.Context, Project) error
	Projects(ctx context.Context, ownerID string) ([]Project, error)
	Project(ctx context.Context, ownerID, id string) (Project, error)
	RenameProject(ctx context.Context, ownerID, id, name string) (Project, error)
	RemoveProject(ctx context.Context, ownerID, id string) error
	SaveProjectSandboxConfig(ctx context.Context, ownerID, projectID string, config ProjectSandboxConfig) error
	WorkspaceRoot(ctx context.Context, ownerID, sessionID string) (string, error)
	SetWorkspaceRoot(ctx context.Context, ownerID, sessionID, projectID, root string) error
}

type PreferenceRepository interface {
	ComposerDefaults(ctx context.Context, ownerID string) (*ComposerDefaults, error)
	SaveComposerDefaults(ctx context.Context, ownerID string, defaults ComposerDefaults) error
	Policy(ctx context.Context, ownerID string) (sandbox.Policy, error)
	SetPolicy(ctx context.Context, ownerID string, choice sandbox.Policy) error
}

type WorkerRepository interface {
	turnClaimer
	sessionProjectReader
	Complete(ctx context.Context, id string, completion Completion) error
	History(ctx context.Context, turn Run, limit int) ([]Exchange, error)
}

type ProjectSandboxRepository interface {
	ProjectSandboxConfig(ctx context.Context, ownerID, projectID string) (*ProjectSandboxConfig, error)
	RememberProjectSandboxProvider(ctx context.Context, ownerID, projectID, provider string) error
}

type sessionProjectReader interface {
	SessionProject(ctx context.Context, ownerID, sessionID string) (string, error)
}

type turnClaimer interface {
	// ClaimNext preserves session order and excludes projects with a running turn.
	ClaimNext(ctx context.Context, now time.Time) (*Run, error)
}

type queuedTurnStopper interface {
	StopQueued(ctx context.Context, ownerID, sessionID, id string, now time.Time) error
}
