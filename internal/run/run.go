// Package run owns agent sessions, turns, and their execution lifecycle.
package run

import (
	"context"
	"errors"
	"time"

	"github.com/OXeu/shinona/internal/sandbox"
)

type Status string

const (
	Queued    Status = "QUEUED"
	Running   Status = "RUNNING"
	Succeeded Status = "SUCCEEDED"
	Failed    Status = "FAILED"
	Stopped   Status = "STOPPED"
)

const (
	MaxPromptCharacters = 8_000
	DefaultSessionTitle = "New session"
	recentSessionLimit  = 30
)

var (
	ErrInvalidPrompt  = errors.New("enter a prompt between 1 and 8,000 characters")
	ErrInvalidTitle   = errors.New("enter a title between 1 and 80 characters")
	ErrSession        = errors.New("session not found")
	ErrActiveRun      = errors.New("this session already has an active turn")
	ErrHistoryChanged = errors.New("the conversation changed before it could be reverted")
	ErrRetryTurn      = errors.New("only the latest failed turn can be retried")
	ErrPolicy         = sandbox.ErrPolicy
	ErrModelResponse  = errors.New("the model did not return a final response")
	ErrModelConfig    = errors.New("set a model endpoint, API key, and default model for this account")
)

type Session struct {
	ID          string
	OwnerID     string
	ProjectID   string
	Title       string
	CustomTitle bool
	TurnCount   int
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Pinned      bool
	Active      bool
	Unread      bool
}

type SessionChanges struct {
	Title  *string
	Pinned *bool
}

type Transcript struct {
	Session Session
	Turns   []Run
}

func (t Transcript) LastOptions() *Options {
	if len(t.Turns) == 0 {
		return nil
	}
	options := t.Turns[len(t.Turns)-1].Payload.Options
	return &options
}

type RevertedTurn struct {
	Session Session
	Prompt  string
	Images  []Image
}

type Run struct {
	ID            string
	OwnerID       string
	SessionID     string
	RetryOf       string // The previous failed attempt, if this turn retries it.
	Sequence      int
	Policy        sandbox.Policy
	Backend       string
	MachineID     string
	Prompt        string
	Payload       Payload
	Steering      []SteeringMessage
	Parts         []TurnPart
	ContextTokens int
	ContextWindow int
	TokenUsage    *TokenUsage
	LiveContext   ContextUsage
	Status        Status
	Output        string
	CreatedAt     time.Time
	StartedAt     *time.Time
	UpdatedAt     time.Time
	RevertedAt    *time.Time
}

type Completion struct {
	Result
	Steering  []SteeringMessage
	Status    Status
	UpdatedAt time.Time
}

type Exchange struct {
	Images   []Image
	Prompt   string
	Output   string
	Steering []SteeringMessage
}

type Input struct {
	OwnerID       string
	Prompt        string
	History       []Exchange
	Payload       Payload
	Steering      *Steering
	ReportParts   func([]TurnPart)
	ReportContext func(ContextUsage)
}

// ContextUsage is a transient reading for the current model request.
type ContextUsage struct {
	Tokens    int32
	Window    int32
	Estimated bool
}

// TokenUsage totals the provider-reported counts for one turn. Input includes
// cache writes, while Cached counts input served from an existing cache.
type TokenUsage struct {
	Input  int32 `json:"input"`
	Cached int32 `json:"cached"`
	Output int32 `json:"output"`
}

type Repository interface {
	CreateSession(context.Context, Session) error
	RecentSessions(ctx context.Context, ownerID string, projectID *string, limit int) ([]Session, error)
	CreateProject(context.Context, Project) error
	Projects(ctx context.Context, ownerID string) ([]Project, error)
	RenameProject(ctx context.Context, ownerID, id, name string) (Project, error)
	RemoveProject(ctx context.Context, ownerID, id string) error
	Transcript(ctx context.Context, ownerID, sessionID string, limit int) (*Transcript, error)
	Attachment(ctx context.Context, ownerID, sessionID, turnID string, index int) (*Image, error)
	SessionProject(ctx context.Context, ownerID, sessionID string) (string, error)
	LatestWorkspaceRun(ctx context.Context, ownerID, sessionID string) (*Run, error)
	UpdateSession(ctx context.Context, ownerID, sessionID string, changes SessionChanges) (Session, error)
	ArchiveSession(ctx context.Context, ownerID, sessionID string, now time.Time) error
	MarkSessionRead(ctx context.Context, ownerID, sessionID string, through int, throughID string) error
	RevertTurn(ctx context.Context, ownerID, sessionID, turnID, expectedHeadID string, now time.Time) (RevertedTurn, error)
	// Enqueue checks ownership and the per-session queue bound atomically.
	Enqueue(ctx context.Context, item Run, firstTitle string, queue bool) (Run, error)
	// EnqueueRetry also verifies that the retry target is still the latest failed turn.
	EnqueueRetry(ctx context.Context, item Run, options RetryOptions) (Run, error)
	StopQueued(ctx context.Context, ownerID, sessionID, id string, now time.Time) error
	History(ctx context.Context, turn Run, limit int) ([]Exchange, error)
	ComposerDefaults(ctx context.Context, ownerID string) (*ComposerDefaults, error)
	SaveComposerDefaults(ctx context.Context, ownerID string, defaults ComposerDefaults) error
	Policy(ctx context.Context, ownerID string) (sandbox.Policy, error)
	SetPolicy(ctx context.Context, ownerID string, choice sandbox.Policy) error
	ClaimNext(ctx context.Context, now time.Time) (*Run, error)
	Complete(ctx context.Context, id string, completion Completion) error
	FailInterrupted(ctx context.Context, now time.Time) error
}

type Agent interface {
	Run(ctx context.Context, input Input, workspace sandbox.Workspace) (Result, error)
}

// Target supplies the sandbox and agent used by one backend. Resolving an empty
// name selects the instance default. A queued turn retains the selected name.
type Target struct {
	Name         string
	Provider     sandbox.Provider
	ResolveAgent func(context.Context, string) (Agent, error)
}

type ResolveTarget func(name string) Target

func (t Target) policies(owner string) []sandbox.Policy {
	if t.Provider == nil || t.ResolveAgent == nil {
		return nil
	}
	return t.Provider.Policies(owner)
}
