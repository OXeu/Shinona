package sqlite

import (
	"context"
	"errors"
	"slices"
	"time"

	"gorm.io/gorm"

	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	latestTranscriptEntries = 128
	latestTranscriptBytes   = 32 << 20
	latestTranscriptPageMax = 4 << 20
)

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
	next      int
}

type latestTranscriptCache struct {
	entries *responsecache.Cache[latestTranscriptKey, *latestTranscriptPage]
}

func newLatestTranscriptCache() latestTranscriptCache {
	return latestTranscriptCache{
		entries: responsecache.New[latestTranscriptKey, *latestTranscriptPage](latestTranscriptEntries, latestTranscriptBytes),
	}
}

func (c *latestTranscriptCache) get(key latestTranscriptKey) *latestTranscriptPage {
	page, _ := c.entries.Get(key)
	return page
}

func (c *latestTranscriptCache) put(key latestTranscriptKey, page *run.Transcript) {
	size := latestTranscriptSize(page.Turns)
	if size > latestTranscriptPageMax {
		return
	}
	entry := &latestTranscriptPage{
		key: key, head: page.Turns[len(page.Turns)-1].ID, turns: cloneTranscriptTurns(page.Turns),
		createdAt: page.Session.CreatedAt, updatedAt: page.Session.UpdatedAt, turnCount: page.Session.TurnCount,
		next: page.NextBeforeSequence,
	}
	c.entries.Put(key, entry, size)
}

func (c *latestTranscriptCache) discard(entry *latestTranscriptPage) {
	c.entries.DeleteIf(entry.key, func(current *latestTranscriptPage) bool { return current == entry })
}

func (r *ConversationStore) validateCachedTranscript(ctx context.Context, key latestTranscriptKey, page *latestTranscriptPage) (*run.Transcript, bool, error) {
	var current struct {
		Session sessionRow `gorm:"embedded"`
		HeadID  string
	}
	// Completed turns are immutable. Revert removes a suffix, while enqueue
	// appends a turn with a fresh ID. Validate the visible head and summary
	// together so this works even with writes from another repository.
	err := sessionScope(r.read.WithContext(ctx), key.owner).
		Select(sessionWithActivity+`, COALESCE((
			SELECT id FROM runs WHERE session_id = agent_sessions.id
			AND owner_id = agent_sessions.owner_id AND reverted_at IS NULL
			ORDER BY sequence DESC LIMIT 1
		), '') AS head_id`, []run.Status{run.Queued, run.Running}).
		First(&current, "id = ?", key.session).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		r.latest.discard(page)
		return nil, true, nil
	}
	if err != nil {
		return nil, false, err
	}
	if current.Session.HasActive || current.HeadID != page.head || current.Session.TurnCount != page.turnCount ||
		!current.Session.CreatedAt.Equal(page.createdAt) || !current.Session.UpdatedAt.Equal(page.updatedAt) {
		r.latest.discard(page)
		return nil, false, nil
	}
	defer timing.Start(ctx, "transcript_cache_hit")()
	return &run.Transcript{
		Session: describeSession(current.Session), Turns: cloneTranscriptTurns(page.turns), NextBeforeSequence: page.next,
	}, true, nil
}

func cloneTranscriptTurns(turns []run.Run) []run.Run {
	copied := slices.Clone(turns)
	for i := range copied {
		turn := &copied[i]
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
	}
	return copied
}

// Estimate retained memory from the strings and slices in the decoded page.
func latestTranscriptSize(turns []run.Run) int {
	size := 1024
	for _, turn := range turns {
		size += 512 + len(turn.ID) + len(turn.OwnerID) + len(turn.SessionID) + len(turn.Backend) +
			len(turn.Policy) + len(turn.Status) + len(turn.Prompt) + len(turn.Output) +
			len(turn.AgentMessages) + len(turn.CompactedContext)
		options := turn.Payload.Options
		size += len(options.Model) + len(options.Permission) + len(options.Thinking) + len(options.MCP)
		for _, server := range options.MCPServers {
			size += 16 + len(server)
		}
		for _, part := range turn.Parts {
			size += 112 + len(part.Kind) + len(part.Text) + len(part.Tool) + len(part.Input) + len(part.Output) + len(part.Status)
		}
		for _, steer := range turn.Steering {
			size += 24 + len(steer.Text)
		}
		for _, image := range turn.Payload.Images {
			size += 48 + len(image.Name) + len(image.MediaType) + len(image.Data)
		}
	}
	return size
}
