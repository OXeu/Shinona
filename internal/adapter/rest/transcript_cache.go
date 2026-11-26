package rest

import (
	"context"
	"fmt"
	"sync"

	"golang.org/x/sync/singleflight"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/responsecache"
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

// Completed turns are immutable and can appear in different transcript pages.
// The repository still checks which turns the current request can see.
type encodedTranscriptTurns struct {
	init    sync.Once
	cache   *responsecache.Cache[transcriptTurnKey, []byte]
	loading singleflight.Group
}

func (c *encodedTranscriptTurns) get(key transcriptTurnKey) []byte {
	c.init.Do(func() {
		c.cache = responsecache.New[transcriptTurnKey, []byte](encodedTranscriptMaxCount, encodedTranscriptCapacity)
	})
	body, _ := c.cache.Get(key)
	return body
}

func (c *encodedTranscriptTurns) add(key transcriptTurnKey, body []byte) {
	size := cap(body) + len(key.owner) + len(key.session) + len(key.id) + 160
	if size <= encodedTranscriptMaxTurn {
		c.cache.Put(key, body, size)
	}
}

func (c *encodedTranscriptTurns) encode(ctx context.Context, turn run.Run) ([]byte, error) {
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
		body, err := marshalTranscriptTurn(turn)
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

func marshalTranscriptTurn(turn run.Run) ([]byte, error) {
	// Concatenating these partial messages appends to the page's repeated turns.
	return proto.Marshal(&sessionpb.TranscriptPage{Turns: []*sessionpb.Turn{sessiontransfer.Turn(turn)}})
}
