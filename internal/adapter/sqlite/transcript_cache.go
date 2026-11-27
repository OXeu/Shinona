package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"time"
	"unsafe"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const latestTranscriptEntries = 128

type latestTranscriptKey struct {
	owner, session string
}

type latestTranscriptPage struct {
	key       latestTranscriptKey
	head      string
	createdAt time.Time
	updatedAt time.Time
	turnCount int
	turns     []run.Run
	earlier   *run.Run
	next      int
}

type latestTranscriptCache struct {
	entries  *responsecache.Cache[latestTranscriptKey, *latestTranscriptPage]
	maxBytes int
}

func newLatestTranscriptCache(bytes int) latestTranscriptCache {
	return latestTranscriptCache{
		entries:  responsecache.New[latestTranscriptKey, *latestTranscriptPage](latestTranscriptEntries, bytes),
		maxBytes: bytes,
	}
}

func (c *latestTranscriptCache) get(key latestTranscriptKey) *latestTranscriptPage {
	page, _ := c.entries.Get(key)
	return page
}

func (c *latestTranscriptCache) put(key latestTranscriptKey, page *run.Transcript) {
	if c.maxBytes == 0 {
		return
	}
	// The query budgets stored bytes. Charge decoded turns and EarlierTurn to
	// the total cache budget without applying the query's byte limit again.
	size := latestTranscriptSize(page.Turns, page.EarlierTurn) + len(key.owner) + len(key.session) + len(page.HeadID)
	if size > c.maxBytes {
		return
	}
	entry := &latestTranscriptPage{
		key: key, head: page.HeadID, turns: cloneTranscriptTurns(page.Turns),
		createdAt: page.Session.CreatedAt, updatedAt: page.Session.UpdatedAt, turnCount: page.Session.TurnCount,
		next: page.NextBeforeSequence,
	}
	if page.EarlierTurn != nil {
		entry.earlier = new(cloneTranscriptTurn(*page.EarlierTurn))
	}
	c.entries.Put(key, entry, size)
}

func (c *latestTranscriptCache) discard(entry *latestTranscriptPage) {
	c.entries.DeleteIf(entry.key, func(current *latestTranscriptPage) bool { return current == entry })
}

func (r *ConversationStore) TranscriptState(ctx context.Context, owner, id string) (*run.TranscriptState, error) {
	return transcriptState(ctx, r.read.Statement.ConnPool, owner, id)
}

type transcriptHeadRow struct {
	Session sessionRow
	HeadID  string
}

func (r transcriptHeadRow) state() run.TranscriptState {
	return run.TranscriptState{Session: describeSession(r.Session), HeadID: r.HeadID}
}

func transcriptState(ctx context.Context, conn gorm.ConnPool, owner, id string) (*run.TranscriptState, error) {
	current, err := readTranscriptHead(ctx, conn, owner, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	state := current.state()
	return &state, nil
}

// Completed turns are immutable. Validate the visible head and summary in the
// same SQLite snapshot, including writes made by another repository.
const transcriptHeadQuery = `SELECT project_id, title, turn_count,
	created_at, updated_at, pinned, custom_title, read_through,
	EXISTS (SELECT 1 FROM runs WHERE session_id = agent_sessions.id AND status IN ('QUEUED', 'RUNNING')),
	COALESCE((SELECT id FROM runs WHERE session_id = agent_sessions.id
		AND owner_id = agent_sessions.owner_id AND reverted_at IS NULL
		ORDER BY sequence DESC LIMIT 1), '')
	FROM agent_sessions WHERE owner_id = ? AND id = ? AND archived_at IS NULL`

func readTranscriptHead(ctx context.Context, conn gorm.ConnPool, owner, id string) (transcriptHeadRow, error) {
	defer timing.Start(ctx, "sqlite")()
	current := transcriptHeadRow{Session: sessionRow{ID: id, OwnerID: owner}}
	session := &current.Session
	err := conn.QueryRowContext(ctx, transcriptHeadQuery, owner, id).Scan(
		&session.ProjectID, &session.Title, &session.TurnCount,
		&session.CreatedAt, &session.UpdatedAt, &session.Pinned, &session.CustomTitle,
		&session.ReadThrough, &session.HasActive, &current.HeadID,
	)
	return current, err
}

func (r *ConversationStore) cachedTranscriptState(ctx context.Context, key latestTranscriptKey, page *latestTranscriptPage) (*run.TranscriptState, bool, error) {
	state, err := r.TranscriptState(ctx, key.owner, key.session)
	if err != nil {
		return nil, false, err
	}
	if state == nil {
		r.latest.discard(page)
		return nil, true, nil
	}
	if state.Session.Active || state.HeadID != page.head || state.Session.TurnCount != page.turnCount ||
		!state.Session.CreatedAt.Equal(page.createdAt) || !state.Session.UpdatedAt.Equal(page.updatedAt) {
		r.latest.discard(page)
		return nil, false, nil
	}
	return state, true, nil
}

func cloneTranscriptTurns(turns []run.Run) []run.Run {
	copied := slices.Clone(turns)
	for i := range copied {
		copied[i] = cloneTranscriptTurn(copied[i])
	}
	return copied
}

func cloneTranscriptTurn(turn run.Run) run.Run {
	turn.Payload.Images = slices.Clone(turn.Payload.Images)
	turn.Payload.Options.MCPServers = slices.Clone(turn.Payload.Options.MCPServers)
	turn.Steering = slices.Clone(turn.Steering)
	turn.Parts = slices.Clone(turn.Parts)
	turn.AgentMessages = slices.Clone(turn.AgentMessages)
	turn.CompactedContext = slices.Clone(turn.CompactedContext)
	if turn.TokenUsage != nil {
		turn.TokenUsage = new(*turn.TokenUsage)
	}
	if turn.StartedAt != nil {
		turn.StartedAt = new(*turn.StartedAt)
	}
	if turn.RevertedAt != nil {
		turn.RevertedAt = new(*turn.RevertedAt)
	}
	return turn
}

// Estimate retained memory from the strings and slices in the decoded page.
func latestTranscriptSize(turns []run.Run, earlier *run.Run) int {
	size := int(unsafe.Sizeof(latestTranscriptPage{}))
	for _, turn := range turns {
		size += transcriptTurnSize(turn)
	}
	if earlier != nil {
		size += transcriptTurnSize(*earlier)
	}
	return size
}

func transcriptTurnSize(turn run.Run) int {
	size := int(unsafe.Sizeof(run.Run{})) + len(turn.ID) + len(turn.OwnerID) + len(turn.SessionID) + len(turn.Backend) +
		len(turn.Policy) + len(turn.Status) + len(turn.Prompt) + len(turn.Output) +
		len(turn.AgentMessages) + len(turn.CompactedContext)
	size += len(turn.RetryOf)
	if turn.TokenUsage != nil {
		size += int(unsafe.Sizeof(run.TokenUsage{}))
	}
	if turn.StartedAt != nil {
		size += int(unsafe.Sizeof(time.Time{}))
	}
	if turn.RevertedAt != nil {
		size += int(unsafe.Sizeof(time.Time{}))
	}
	options := turn.Payload.Options
	size += len(options.Model) + len(options.Permission) + len(options.Thinking) + len(options.MCP)
	for _, server := range options.MCPServers {
		size += int(unsafe.Sizeof("")) + len(server)
	}
	for _, part := range turn.Parts {
		size += int(unsafe.Sizeof(run.TurnPart{})) + len(part.Kind) + len(part.Text) + len(part.Tool) + len(part.Input) + len(part.Output) + len(part.Status)
	}
	for _, steer := range turn.Steering {
		size += int(unsafe.Sizeof(run.SteeringMessage{})) + len(steer.Text)
	}
	for _, image := range turn.Payload.Images {
		size += int(unsafe.Sizeof(run.Image{})) + len(image.Name) + len(image.MediaType) + len(image.Data)
	}
	return size
}
