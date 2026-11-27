package rest

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

type sessionSummary struct {
	ID           string    `json:"id"`
	ProjectID    *string   `json:"projectId"`
	Title        string    `json:"title"`
	DefaultTitle bool      `json:"defaultTitle"`
	TurnCount    int       `json:"turnCount"`
	UpdatedAt    time.Time `json:"updatedAt"`
	Pinned       bool      `json:"pinned"`
	Active       bool      `json:"active"`
	Unread       bool      `json:"unread"`
}

func publicSession(session run.Session) sessionSummary {
	var project *string
	if session.ProjectID != "" {
		project = &session.ProjectID
	}
	return sessionSummary{
		ID: session.ID, ProjectID: project, Title: session.Title,
		DefaultTitle: session.TurnCount == 0 && !session.CustomTitle, TurnCount: session.TurnCount,
		UpdatedAt: session.UpdatedAt, Pinned: session.Pinned, Active: session.Active, Unread: session.Unread,
	}
}

type sessionsResponse struct {
	Projects []projectResponse `json:"projects"`
	Sessions []sessionSummary  `json:"sessions"`
}

func publicSessionList(snapshot run.SessionListSnapshot) sessionsResponse {
	result := sessionsResponse{
		Projects: publicProjects(snapshot.Projects),
		Sessions: make([]sessionSummary, len(snapshot.Sessions)),
	}
	for i, item := range snapshot.Sessions {
		result.Sessions[i] = publicSession(item)
	}
	return result
}

type turnResponse struct {
	ID            string                `json:"id"`
	Sequence      int                   `json:"sequence"`
	Prompt        string                `json:"prompt"`
	Status        run.Status            `json:"status"`
	Policy        *sandbox.Policy       `json:"policy"`
	Output        string                `json:"output"`
	Parts         []turnPartResponse    `json:"parts"`
	Model         string                `json:"model"`
	Attachments   []string              `json:"attachments"`
	Steering      []run.SteeringMessage `json:"steering"`
	ContextTokens int                   `json:"contextTokens"`
	ContextWindow int                   `json:"contextWindow"`
	TokenUsage    *run.TokenUsage       `json:"tokenUsage"`
	LiveContext   *run.ContextUsage     `json:"liveContext"`
	CreatedAt     time.Time             `json:"createdAt"`
	StartedAt     *time.Time            `json:"startedAt"`
	UpdatedAt     time.Time             `json:"updatedAt"`
}

type turnPartResponse struct {
	Kind            string `json:"kind"`
	Text            string `json:"text"`
	Tool            string `json:"tool"`
	Input           string `json:"input"`
	Output          string `json:"output"`
	Status          string `json:"status"`
	EstimatedTokens int32  `json:"estimatedTokens"`
}

func publicTurn(item run.Run) turnResponse {
	attachments := make([]string, len(item.Payload.Images))
	for i, image := range item.Payload.Images {
		attachments[i] = image.Name
	}
	parts := make([]turnPartResponse, len(item.Parts))
	for i, part := range item.Parts {
		parts[i] = turnPartResponse(part)
	}
	var liveContext *run.ContextUsage
	if item.LiveContext.Tokens > 0 {
		liveContext = &item.LiveContext
	}
	return turnResponse{
		ID: item.ID, Sequence: item.Sequence, Prompt: item.Prompt, Status: item.Status,
		Policy: optionalPolicy(item.Policy), Output: item.Output, Parts: parts,
		Model: item.Payload.Options.Model, Attachments: attachments, Steering: nonNil(item.Steering),
		ContextTokens: item.ContextTokens, ContextWindow: item.ContextWindow, TokenUsage: item.TokenUsage,
		LiveContext: liveContext, CreatedAt: item.CreatedAt, StartedAt: item.StartedAt, UpdatedAt: item.UpdatedAt,
	}
}

func nonNil[T any](items []T) []T {
	if items == nil {
		return []T{}
	}
	return items
}

func optionalPolicy(value sandbox.Policy) *sandbox.Policy {
	if value == "" {
		return nil
	}
	return &value
}

type transcriptRequest struct {
	ID             string `uri:"id"`
	BeforeSequence *int   `form:"beforeSequence"`
	Paged          *bool  `form:"paged"`
}

func (r *handler) transcript(c *gin.Context) {
	zstd := httpserver.NegotiateZstd(c.Writer, c.Request)
	var args transcriptRequest
	if !bind(c, &args, false) {
		return
	}
	paged := args.Paged == nil || *args.Paged
	var before int
	if args.BeforeSequence != nil {
		before = *args.BeforeSequence
		if !paged || before <= 0 {
			writeError(c, codedError{errors.New("invalid transcript cursor"), "INVALID_TRANSCRIPT_CURSOR"})
			return
		}
	}
	ctx := c.Request.Context()
	if !paged {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, fullTranscriptTimeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		deadline, _ := ctx.Deadline()
		controller := http.NewResponseController(c.Writer)
		if err := controller.SetWriteDeadline(deadline.Add(transcriptErrorWriteTime)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			r.logger.Debug("set transcript export write deadline", "error", err)
		}
	}
	owner := auth.CurrentUser(ctx).ID
	settings, err := r.service.Settings(ctx, owner)
	if err != nil {
		r.writeTranscriptFailure(c, err)
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")
	key := (transcriptPageKey{owner: owner, session: args.ID, before: before, full: !paged, zstd: zstd}).withSettings(settings)
	cacheKey, cacheable, err := r.lookupTranscriptKey(ctx, key)
	if err != nil {
		r.writeTranscriptFailure(c, err)
		return
	}
	if cacheable && r.serveCachedTranscript(c, cacheKey) {
		return
	}
	slots := r.fullTranscript
	if paged {
		slots = r.pagedTranscript
	}
	wait := timing.Start(ctx, "transcript_wait")
	waited := !slots.TryAcquire(1)
	if waited {
		err = slots.Acquire(ctx, 1)
	}
	wait()
	if err != nil {
		r.writeTranscriptFailure(c, err)
		return
	}
	defer slots.Release(1)

	// A request waiting for a slot can reuse a response filled for the
	// same snapshot it validated when it first looked in the cache.
	if waited && cacheable && r.serveCachedTranscript(c, cacheKey) {
		return
	}
	if paged {
		err = r.pagedTranscriptResponse(c, key, settings)
	} else {
		err = r.fullTranscriptResponse(c, key, settings)
	}
	if err != nil {
		r.writeTranscriptFailure(c, err)
	}
}

func (r *handler) writeTranscriptFailure(c *gin.Context, err error) {
	if c.Writer.Written() {
		r.logger.Debug("write transcript response", "error", err)
		return
	}
	if errors.Is(c.Request.Context().Err(), context.Canceled) {
		return
	}
	for _, name := range []string{"Content-Type", "Content-Encoding", "Content-Length", "Trailer"} {
		c.Writer.Header().Del(name)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(c.Request.Context().Err(), context.DeadlineExceeded) {
		reject(c, http.StatusGatewayTimeout, "TRANSCRIPT_EXPORT_TIMEOUT", "The transcript request timed out.")
		return
	}
	writeError(c, r.apiError(err))
}

func (r *handler) createSession(ctx context.Context, args struct {
	ProjectID *string `json:"projectId"`
}) (sessionSummary, error) {
	item, err := r.service.CreateSession(ctx, auth.CurrentUser(ctx).ID, value(args.ProjectID))
	return publicSession(item), r.apiError(err)
}

func (r *handler) updateSession(ctx context.Context, args struct {
	ID     string  `uri:"id" json:"-"`
	Title  *string `json:"title"`
	Pinned *bool   `json:"pinned"`
}) (sessionSummary, error) {
	item, err := r.service.UpdateSession(ctx, auth.CurrentUser(ctx).ID, args.ID, run.SessionChanges{
		Title: args.Title, Pinned: args.Pinned,
	})
	return publicSession(item), r.apiError(err)
}

type resourceID struct {
	ID string `uri:"id" json:"-"`
}

func (r *handler) archiveSession(ctx context.Context, args resourceID) error {
	err := r.service.ArchiveSession(ctx, auth.CurrentUser(ctx).ID, args.ID)
	return r.apiError(err)
}

func (r *handler) markSessionRead(ctx context.Context, args struct {
	ID        string  `uri:"id" json:"-"`
	Through   int     `json:"through"`
	ThroughID *string `json:"throughId"`
}) error {
	err := r.service.MarkSessionRead(ctx, auth.CurrentUser(ctx).ID, args.ID, args.Through, value(args.ThroughID))
	return r.apiError(err)
}

type sandboxSettings struct {
	Available []sandbox.Policy `json:"available"`
	Selected  *sandbox.Policy  `json:"selected"`
}

func (r *handler) SandboxSettings(ctx context.Context) (sandboxSettings, error) {
	settings, err := r.service.Settings(ctx, auth.CurrentUser(ctx).ID)
	return sandboxSettings{Available: nonNil(settings.Available), Selected: optionalPolicy(settings.Selected)}, r.apiError(err)
}

func (r *handler) setSandboxPolicy(ctx context.Context, args struct {
	Policy sandbox.Policy `json:"policy"`
}) (sandboxSettings, error) {
	if !slices.Contains([]sandbox.Policy{sandbox.Shared, sandbox.Fresh}, args.Policy) {
		return sandboxSettings{}, r.apiError(run.ErrPolicy)
	}
	settings, err := r.service.SetPolicy(ctx, auth.CurrentUser(ctx).ID, args.Policy)
	return sandboxSettings{Available: nonNil(settings.Available), Selected: optionalPolicy(settings.Selected)}, r.apiError(err)
}
