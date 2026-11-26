package rest

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
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

func (r *handler) sessions(ctx context.Context, projectID *string) (sessionsResponse, error) {
	projects, err := r.Projects(ctx)
	if err != nil {
		return sessionsResponse{}, err
	}
	items, err := r.service.RecentSessions(ctx, auth.CurrentUser(ctx).ID, projectID)
	if err != nil {
		return sessionsResponse{}, r.apiError(err)
	}
	result := sessionsResponse{Projects: projects, Sessions: make([]sessionSummary, len(items))}
	for i, item := range items {
		result.Sessions[i] = publicSession(item)
	}
	return result, nil
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
	slots := r.fullTranscript
	if paged {
		slots = r.pagedTranscript
	}
	ctx := c.Request.Context()
	wait := timing.Start(ctx, "transcript_wait")
	err := slots.Acquire(ctx, 1)
	wait()
	if err != nil {
		return
	}
	defer slots.Release(1)

	owner := auth.CurrentUser(ctx).ID
	load := timing.Start(ctx, "transcript_load")
	transcript, err := r.service.TranscriptSnapshot(ctx, owner, args.ID, paged, before)
	load()
	if err != nil {
		writeError(c, r.apiError(err))
		return
	}
	settings, err := r.service.Settings(ctx, owner)
	if err != nil {
		writeError(c, r.apiError(err))
		return
	}
	c.Header("X-Content-Type-Options", "nosniff")

	if !paged && transcript != nil && len(transcript.Turns) > run.TranscriptPageSize {
		if err := r.streamTranscript(c, transcript, settings); err != nil && ctx.Err() == nil {
			if !c.Writer.Written() {
				c.Header("Content-Type", "")
				writeError(c, r.apiError(err))
			} else {
				r.logger.Debug("write transcript response", "error", err)
			}
		}
		return
	}
	var body bytes.Buffer
	encode := timing.Start(ctx, "transcript_encode")
	if transcript != nil {
		size := 512 + len(transcript.Session.Title)
		for i, turn := range transcript.Turns {
			size += 512 + len(turn.Prompt) + len(turn.Output) + len(transcript.StoredTurns[i].Parts)
		}
		body.Grow(size)
	}
	err = r.writeTranscript(ctx, &body, transcript, settings)
	encode()
	if err != nil {
		writeError(c, r.apiError(err))
		return
	}
	c.Data(http.StatusOK, httpserver.ProtobufMediaType, body.Bytes())
}

// Stream one Protobuf message without buffering the entire response.
func (r *handler) streamTranscript(c *gin.Context, transcript *run.TranscriptSnapshot, settings run.Settings) error {
	ctx, finish := httpserver.StreamTiming(c.Writer, c.Request.Context())
	defer finish()
	defer timing.Start(ctx, "transcript_write")()
	c.Header("Content-Type", httpserver.ProtobufMediaType)
	buffer := bufio.NewWriterSize(c.Writer, 32<<10)
	if err := r.writeTranscript(ctx, buffer, transcript, settings); err != nil {
		return err
	}
	return buffer.Flush()
}

func (r *handler) writeTranscript(ctx context.Context, w io.Writer, transcript *run.TranscriptSnapshot, settings run.Settings) error {
	meta := &sessionpb.TranscriptPage{SandboxSettings: sessiontransfer.SandboxSettings(settings)}
	if transcript != nil {
		meta.Session = sessiontransfer.Summary(transcript.Session)
		meta.NextBeforeSequence = int32(transcript.NextBeforeSequence)
		meta.LastOptions = sessiontransfer.RunOptions(transcript.LastOptions())
	}
	header, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	if _, err := w.Write(header); err != nil {
		return err
	}
	if transcript != nil {
		for i, turn := range transcript.Turns {
			if err := ctx.Err(); err != nil {
				return err
			}
			body, err := r.transcriptTurns.encode(ctx, turn, transcript.StoredTurns[i])
			if err != nil {
				return err
			}
			if _, err := w.Write(body); err != nil {
				return err
			}
		}
	}
	end, err := proto.Marshal(&sessionpb.TranscriptPage{Complete: true})
	if err != nil {
		return err
	}
	_, err = w.Write(end)
	return err
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

func (r *handler) archiveSession(ctx context.Context, args resourceID) (bool, error) {
	err := r.service.ArchiveSession(ctx, auth.CurrentUser(ctx).ID, args.ID)
	return err == nil, r.apiError(err)
}

func (r *handler) markSessionRead(ctx context.Context, args struct {
	ID        string  `uri:"id" json:"-"`
	Through   int     `json:"through"`
	ThroughID *string `json:"throughId"`
}) (bool, error) {
	err := r.service.MarkSessionRead(ctx, auth.CurrentUser(ctx).ID, args.ID, args.Through, value(args.ThroughID))
	return err == nil, r.apiError(err)
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
