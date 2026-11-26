package rest

import (
	"container/list"
	"context"
	stdjson "encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/segmentio/encoding/json"
	"golang.org/x/sync/singleflight"

	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	encodedTranscriptCapacity = 32 << 20
	encodedTranscriptMaxTurn  = 2 << 20
	encodedTranscriptMaxCount = 2048
)

type transcriptTurnKey struct {
	owner, session, id string
	status             run.Status
	updatedAt          int64
}

type encodedTranscriptTurn struct {
	key  transcriptTurnKey
	body []byte
	size int
}

// Completed turns are immutable and can appear in different transcript pages.
// The repository still checks which turns the current request can see.
type encodedTranscriptTurns struct {
	mu      sync.Mutex
	entries map[transcriptTurnKey]*list.Element
	order   list.List
	size    int
	loading singleflight.Group
}

func (c *encodedTranscriptTurns) get(key transcriptTurnKey) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element := c.entries[key]; element != nil {
		c.order.MoveToFront(element)
		return element.Value.(*encodedTranscriptTurn).body
	}
	return nil
}

func (c *encodedTranscriptTurns) add(key transcriptTurnKey, body []byte) {
	size := cap(body) + len(key.owner) + len(key.session) + len(key.id) + 160
	if size > encodedTranscriptMaxTurn {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries == nil {
		c.entries = make(map[transcriptTurnKey]*list.Element)
	}
	for len(c.entries) >= encodedTranscriptMaxCount || c.size+size > encodedTranscriptCapacity {
		oldest := c.order.Back()
		entry := oldest.Value.(*encodedTranscriptTurn)
		delete(c.entries, entry.key)
		c.order.Remove(oldest)
		c.size -= entry.size
	}
	c.entries[key] = c.order.PushFront(&encodedTranscriptTurn{key: key, body: body, size: size})
	c.size += size
}

func (c *encodedTranscriptTurns) encode(ctx context.Context, turn run.Run, stored run.StoredTurnJSON) ([]byte, error) {
	if !turn.Status.Completed() {
		return json.Marshal(publicTurn(turn))
	}
	key := transcriptTurnKey{
		owner: turn.OwnerID, session: turn.SessionID, id: turn.ID, status: turn.Status, updatedAt: turn.UpdatedAt.UnixNano(),
	}
	if body := c.get(key); body != nil {
		return body, nil
	}
	flightKey := fmt.Sprintf("%q/%q/%q/%s/%d", key.owner, key.session, key.id, key.status, key.updatedAt)
	value, err, _ := c.loading.Do(flightKey, func() (any, error) {
		if body := c.get(key); body != nil {
			return body, nil
		}
		defer timing.Start(ctx, "transcript_turn_encode")()
		body, err := encodeStoredTurn(turn, stored)
		if err != nil {
			return nil, fmt.Errorf("encode transcript turn %s: %w", turn.ID, err)
		}
		c.add(key, body)
		return body, nil
	})
	if err != nil {
		return nil, err
	}
	return value.([]byte), nil
}

func encodeStoredTurn(turn run.Run, stored run.StoredTurnJSON) ([]byte, error) {
	parts := strings.TrimSpace(stored.Parts)
	if parts == "" || parts == "null" {
		parts = "[]"
	}
	if parts[0] != '[' {
		return nil, fmt.Errorf("stored transcript parts must be a JSON array")
	}
	usage, err := storedTokenUsage(stored.TokenUsage)
	if err != nil {
		return nil, err
	}
	return stdjson.Marshal(struct {
		turnResponse
		Parts      stdjson.RawMessage `json:"parts"`
		TokenUsage json.RawMessage    `json:"tokenUsage"`
	}{publicTurn(turn), stdjson.RawMessage(parts), usage})
}

func storedTokenUsage(value string) (json.RawMessage, error) {
	if value == "" {
		return nil, nil
	}
	// SQLite writes the same three int32 fields as the REST response. Older
	// or differently formatted values use the typed path to preserve defaults.
	fields, opened := strings.CutPrefix(value, `{"input":`)
	input, fields, hasCached := strings.Cut(fields, `,"cached":`)
	cached, output, hasOutput := strings.Cut(fields, `,"output":`)
	output, closed := strings.CutSuffix(output, "}")
	if opened && hasCached && hasOutput && closed &&
		canonicalInt32(input) && canonicalInt32(cached) && canonicalInt32(output) {
		return json.RawMessage(value), nil
	}
	var usage *run.TokenUsage
	if err := stdjson.Unmarshal([]byte(value), &usage); err != nil {
		return nil, err
	}
	return json.Marshal(usage)
}

func canonicalInt32(value string) bool {
	n, err := strconv.ParseInt(value, 10, 32)
	if err != nil {
		return false
	}
	var buffer [11]byte
	return string(strconv.AppendInt(buffer[:0], n, 10)) == value
}
