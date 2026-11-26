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

func (s Status) Completed() bool {
	return s == Succeeded || s == Failed || s == Stopped
}

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
	ErrModelIdle      = errors.New("the model endpoint stopped sending response data")
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

func (s Session) TitleAfterAppend(firstTitle string) string {
	if s.TurnCount == 0 && !s.CustomTitle {
		return firstTitle
	}
	return s.Title
}

func (s Session) TitleAfterRemoval(remaining int) string {
	if remaining == 0 && !s.CustomTitle {
		return DefaultSessionTitle
	}
	return s.Title
}

// CheckRetry validates the visible turn against a session read in the same transaction.
func (s Session) CheckRetry(sequence int, status Status) error {
	if sequence != s.TurnCount || status != Failed {
		return ErrRetryTurn
	}
	return nil
}

type SessionListSnapshot struct {
	Revision int64
	Projects []Project
	Sessions []Session
}

type SessionChanges struct {
	Title  *string
	Pinned *bool
}

const TranscriptPageSize = 30
const MaxLiveTranscriptTurns = 256
const maxActiveSessionTurns = 8

func CheckTurnAdmission(active int64, queue bool) error {
	if active > 0 && !queue {
		return ErrActiveRun
	}
	if active >= maxActiveSessionTurns {
		return ErrQueueFull
	}
	return nil
}

const MaxTranscriptExportTurns = 5000

var ErrTranscriptExportLimit = errors.New("the transcript is too large for an unpaged response")

// TranscriptState identifies an inactive transcript and its session summary.
// Completed turns are immutable, and a revert removes a suffix of them.
type TranscriptState struct {
	Session Session
	HeadID  string
}

func (t TranscriptState) CheckRevert(expectedHeadID string) error {
	if t.Session.Active {
		return ErrActiveRun
	}
	if t.HeadID == "" || t.HeadID != expectedHeadID {
		return ErrHistoryChanged
	}
	return nil
}

type Transcript struct {
	TranscriptState
	Turns              []Run
	NextBeforeSequence int
}

type LiveTranscriptWindow uint8

const (
	LiveLatestPage LiveTranscriptWindow = iota
	LiveFullHistory
)

func (w LiveTranscriptWindow) Limit() int {
	if w == LiveFullHistory {
		return MaxLiveTranscriptTurns
	}
	return TranscriptPageSize
}

type TurnVersion struct {
	ID        string
	UpdatedAt time.Time
	Status    Status
}

func (v TurnVersion) NeedsSync(known map[string]TurnVersion) bool {
	previous, ok := known[v.ID]
	return !ok || v.Status == Running || v.Status != previous.Status || !v.UpdatedAt.Equal(previous.UpdatedAt)
}

type TranscriptUpdate struct {
	Session            Session
	Versions           []TurnVersion
	ChangedTurns       []Run
	LastOptions        *Options
	NextBeforeSequence int
}

type Attachment struct {
	MediaType string
	Data      []byte
}

// TranscriptStream reads forward from one durable snapshot. Next is valid
// for the duration of the repository callback and returns nil at the end.
type TranscriptStream struct {
	Session     *Session
	HeadID      string
	LastOptions *Options
	TurnCount   int
	Next        func() ([]Run, error)
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
	ID               string
	OwnerID          string
	SessionID        string
	RetryOf          string // The previous failed attempt, if this turn retries it.
	Sequence         int
	Policy           sandbox.Policy
	Backend          string
	Prompt           string
	Payload          Payload
	Steering         []SteeringMessage
	Parts            []TurnPart
	AgentMessages    []byte `json:"-"`
	CompactedContext []byte `json:"-"`
	ContextTokens    int
	ContextWindow    int
	TokenUsage       *TokenUsage
	LiveContext      ContextUsage
	Status           Status
	Output           string
	CreatedAt        time.Time
	StartedAt        *time.Time
	UpdatedAt        time.Time
	RevertedAt       *time.Time
}

type Completion struct {
	Result
	Steering  []SteeringMessage
	Status    Status
	UpdatedAt time.Time
}

type Exchange struct {
	Images           []Image
	Prompt           string
	Output           string
	Steering         []SteeringMessage
	AgentMessages    []byte
	CompactedContext []byte
}

type Input struct {
	OwnerID       string
	SessionID     string
	Prompt        string
	History       []Exchange
	Payload       Payload
	Steering      *Steering
	ReportParts   func([]TurnPart)
	ReportContext func(ContextUsage)
}

// ContextUsage is a transient reading for the current model request.
type ContextUsage struct {
	Tokens    int32 `json:"tokens"`
	Window    int32 `json:"window"`
	Estimated bool  `json:"estimated"`
}

// TokenUsage totals the provider-reported counts for one turn. Input includes
// cache writes, while Cached counts input served from an existing cache.
type TokenUsage struct {
	Input  int32 `json:"input"`
	Cached int32 `json:"cached"`
	Output int32 `json:"output"`
}

type Agent interface {
	Run(ctx context.Context, input Input, workspace sandbox.Workspace) (Result, error)
}

// Target supplies the sandbox and agent used by one backend. Resolving an empty
// name selects the instance default. A queued turn retains the selected name.
type Target struct {
	Name         string
	Label        string
	Provider     sandbox.Provider
	ResolveAgent func(context.Context, string) (Agent, error)
	Defaults     ProjectSandboxConfig
}

type ResolveTarget func(ctx context.Context, owner, name string) (Target, error)
type ListProjectTargets func(ctx context.Context, owner string) ([]Target, error)

func (t Target) policies(owner string) []sandbox.Policy {
	if t.Provider == nil || t.ResolveAgent == nil {
		return nil
	}
	return t.Provider.Policies(owner)
}
