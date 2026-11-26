package rest

import (
	"container/list"
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
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
		return marshalTranscriptTurn(turn)
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
	if stored.Parts != "" {
		if err := json.Unmarshal([]byte(stored.Parts), &turn.Parts); err != nil {
			return nil, err
		}
	}
	if stored.TokenUsage != "" {
		if err := json.Unmarshal([]byte(stored.TokenUsage), &turn.TokenUsage); err != nil {
			return nil, err
		}
	}
	return marshalTranscriptTurn(turn)
}

func marshalTranscriptTurn(turn run.Run) ([]byte, error) {
	// Concatenating these partial messages appends to the page's repeated turns.
	return proto.Marshal(&sessionpb.TranscriptPage{Turns: []*sessionpb.Turn{sessiontransfer.Turn(turn)}})
}
