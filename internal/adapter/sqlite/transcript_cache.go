package sqlite

import (
	"container/list"
	"context"
	"errors"
	"slices"
	"sync"
	"time"

	"gorm.io/gorm"

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
	retainJSON     bool
}

type latestTranscriptPage struct {
	key       latestTranscriptKey
	head      string
	createdAt time.Time
	updatedAt time.Time
	turnCount int
	turns     []run.Run
	stored    []run.StoredTurnJSON
	next      int
	bytes     int
}

type latestTranscriptCache struct {
	mu      sync.Mutex
	entries map[latestTranscriptKey]*list.Element
	order   list.List
	bytes   int
}

func (c *latestTranscriptCache) get(key latestTranscriptKey) *latestTranscriptPage {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		return element.Value.(*latestTranscriptPage)
	}
	return nil
}

func (c *latestTranscriptCache) put(key latestTranscriptKey, page *run.TranscriptSnapshot) {
	size := latestTranscriptSize(page.Turns)
	for _, fields := range page.StoredTurns {
		size += 32 + len(fields.Parts) + len(fields.TokenUsage)
	}
	if size > latestTranscriptPageMax {
		return
	}
	entry := &latestTranscriptPage{
		key: key, head: page.Turns[len(page.Turns)-1].ID, turns: cloneTranscriptTurns(page.Turns),
		stored:    slices.Clone(page.StoredTurns),
		createdAt: page.Session.CreatedAt, updatedAt: page.Session.UpdatedAt, turnCount: page.Session.TurnCount,
		next: page.NextBeforeSequence, bytes: size,
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[latestTranscriptKey]*list.Element)
	}
	if old := c.entries[key]; old != nil {
		c.removeLocked(old)
	}
	for len(c.entries) >= latestTranscriptEntries || c.bytes+size > latestTranscriptBytes {
		c.removeLocked(c.order.Back())
	}
	c.entries[key] = c.order.PushFront(entry)
	c.bytes += size
}

func (c *latestTranscriptCache) discard(entry *latestTranscriptPage) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[entry.key]; element != nil && element.Value == entry {
		c.removeLocked(element)
	}
}

func (c *latestTranscriptCache) removeLocked(element *list.Element) {
	entry := element.Value.(*latestTranscriptPage)
	delete(c.entries, entry.key)
	c.bytes -= entry.bytes
	c.order.Remove(element)
}

func (r *Repository) validateCachedTranscript(ctx context.Context, key latestTranscriptKey, page *latestTranscriptPage) (*run.TranscriptSnapshot, bool, error) {
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
	return &run.TranscriptSnapshot{
		Transcript: run.Transcript{
			Session: describeSession(current.Session), Turns: cloneTranscriptTurns(page.turns), NextBeforeSequence: page.next,
		},
		StoredTurns: slices.Clone(page.stored),
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
			len(turn.MachineID) + len(turn.Policy) + len(turn.Status) + len(turn.Prompt) + len(turn.Output) +
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
