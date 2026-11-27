package rest

import (
	"bufio"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/sandbox"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	maxCachedTranscriptResponse = 512 << 10
	maxFullTranscriptBytes      = 128 << 20
	fullTranscriptTimeout       = 45 * time.Second
	transcriptErrorWriteTime    = 2 * time.Second
)

// Fields of sessionpb.TranscriptPage written around the cached Turn messages.
const (
	pageTurnField     = 5
	pageCompleteField = 6
)

type transcriptPageKey struct {
	owner, session, policies                 string
	selected                                 sandbox.Policy
	before                                   int
	head, project, title                     string
	updatedAt                                int64
	updatedNanos, zoneOffset                 int
	turnCount                                int
	defaultTitle, pinned, unread, zstd, full bool
}

type transcriptPageBody struct {
	contentType string
	encoding    string
	data        []byte
}

func (key transcriptPageKey) withSettings(settings run.Settings) transcriptPageKey {
	key.selected = settings.Selected
	var policies strings.Builder
	for _, policy := range settings.Available {
		policies.WriteString(string(policy))
		policies.WriteByte(',')
	}
	key.policies = policies.String()
	return key
}

func (key transcriptPageKey) withState(state run.TranscriptState) transcriptPageKey {
	session := state.Session
	key.head, key.project, key.title = state.HeadID, session.ProjectID, session.Title
	key.updatedAt, key.updatedNanos = session.UpdatedAt.Unix(), session.UpdatedAt.Nanosecond()
	_, key.zoneOffset = session.UpdatedAt.Zone()
	key.turnCount, key.pinned, key.unread = session.TurnCount, session.Pinned, session.Unread
	key.defaultTitle = session.TurnCount == 0 && !session.CustomTitle
	return key
}

func (r *handler) lookupTranscriptKey(ctx context.Context, key transcriptPageKey) (transcriptPageKey, bool, error) {
	if r.transcriptPages == nil {
		return key, false, nil
	}
	state, err := r.service.TranscriptState(ctx, key.owner, key.session)
	if err != nil || state == nil || state.Session.Active {
		return key, false, err
	}
	return key.withState(*state), true, nil
}

func (r *handler) serveCachedTranscript(c *gin.Context, key transcriptPageKey) bool {
	lookup := timing.Start(c.Request.Context(), "transcript_response_cache_hit")
	hit, ok := r.transcriptPages.Get(key)
	if !ok {
		return false
	}
	lookup()
	c.Header("Content-Encoding", hit.encoding)
	c.Header("Cache-Control", "private, no-store")
	c.Data(http.StatusOK, hit.contentType, hit.data)
	return true
}

func (r *handler) saveTranscriptResponse(c *gin.Context, key transcriptPageKey, state run.TranscriptState, capture *boundedCapture) {
	if capture == nil || capture.overflow {
		return
	}
	// Use the state captured with these turns, even if a concurrent write
	// changed the session after the initial cache lookup.
	key = key.withState(state)
	encoding := c.Writer.Header().Get("Content-Encoding")
	r.transcriptPages.Put(key, transcriptPageBody{c.Writer.Header().Get("Content-Type"), encoding, capture.Bytes()},
		capture.Cap()+len(key.owner)+len(key.session)+len(key.policies)+len(key.head)+len(key.project)+len(key.title)+len(key.selected))
}

func (r *handler) pagedTranscriptResponse(c *gin.Context, key transcriptPageKey, settings run.Settings) error {
	ctx := c.Request.Context()
	load := timing.Start(ctx, "transcript_load")
	page, err := r.service.TranscriptPage(ctx, key.owner, key.session, key.before)
	load()
	if err != nil {
		return err
	}
	meta := &sessionpb.TranscriptPage{SandboxSettings: sessiontransfer.SandboxSettings(settings)}
	var capture *boundedCapture
	var turns [][]byte
	if page != nil {
		meta.Session = sessiontransfer.Summary(page.Session)
		meta.LastOptions = sessiontransfer.RunOptions(page.LastOptions())
		meta.NextBeforeSequence = int32(page.NextBeforeSequence)
		if page.EarlierTurn != nil {
			meta.EarlierTurn = sessiontransfer.Turn(*page.EarlierTurn)
		}
		turns = make([][]byte, len(page.Turns))
		for i, turn := range page.Turns {
			if err := ctx.Err(); err != nil {
				return err
			}
			body, err := r.transcriptTurns.encode(ctx, turn)
			if err != nil {
				return err
			}
			turns[i] = body
		}
	}
	header, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	var completion [16]byte
	end := protowire.AppendVarint(protowire.AppendTag(completion[:0], pageCompleteField, protowire.VarintType), 1)
	size := len(header) + len(end)
	for _, turn := range turns {
		size += protowire.SizeTag(pageTurnField) + protowire.SizeBytes(len(turn))
	}
	compress := key.zstd && size >= httpserver.MinimumCompressSize
	if r.transcriptPages != nil && page != nil && !page.Session.Active && (compress || size <= maxCachedTranscriptResponse) {
		capture = &boundedCapture{}
	}
	if !compress {
		c.Header("Content-Length", strconv.Itoa(size))
	}
	err = writeTranscriptResponse(c, httpserver.ProtobufMediaType, compress, capture, func(w *bufio.Writer) error {
		if _, err := w.Write(header); err != nil {
			return err
		}
		var prefix [16]byte
		for _, turn := range turns {
			tag := protowire.AppendTag(prefix[:0], pageTurnField, protowire.BytesType)
			if _, err := w.Write(protowire.AppendVarint(tag, uint64(len(turn)))); err != nil {
				return err
			}
			if _, err := w.Write(turn); err != nil {
				return err
			}
		}
		// A valid Protobuf field prefix may omit turns. Mark completion
		// only after the last turn has been written.
		_, err := w.Write(end)
		return err
	})
	if err == nil && capture != nil {
		r.saveTranscriptResponse(c, key, page.TranscriptState, capture)
	}
	return err
}

type boundedCapture struct {
	chunks   [][]byte
	length   int
	overflow bool
}

func (b *boundedCapture) Bytes() []byte {
	if len(b.chunks) == 1 {
		return b.chunks[0]
	}
	body := make([]byte, 0, b.length)
	for _, chunk := range b.chunks {
		body = append(body, chunk...)
	}
	return body
}

func (b *boundedCapture) Len() int { return b.length }

// Each chunk is allocated to the exact size written.
func (b *boundedCapture) Cap() int { return b.length }

func (b *boundedCapture) Write(data []byte) (int, error) {
	if b.overflow || len(data) == 0 {
		return len(data), nil
	}
	if len(data) > maxCachedTranscriptResponse-b.Len() {
		b.overflow = true
		b.chunks = nil
		b.length = 0
		return len(data), nil
	}
	// Large exports can overrun the cache limit. Avoid reallocating and
	// copying their prefix as it grows, and join only a cacheable response.
	chunk := make([]byte, len(data))
	copy(chunk, data)
	b.chunks = append(b.chunks, chunk)
	b.length += len(data)
	return len(data), nil
}

type transcriptWire struct {
	c      *gin.Context
	sink   io.Writer
	writer io.Writer
	finish func(bool) error
	zstd   bool
}

func (w *transcriptWire) Write(data []byte) (int, error) {
	if w.writer == nil {
		w.writer = w.sink
		if w.zstd && len(data) >= httpserver.MinimumCompressSize {
			w.writer, w.finish = httpserver.NewZstdResponse(w.c.Writer, w.sink)
		}
	}
	return w.writer.Write(data)
}

func writeTranscriptResponse(c *gin.Context, mediaType string, zstd bool, capture *boundedCapture, write func(*bufio.Writer) error) (err error) {
	c.Header("Content-Type", mediaType)
	c.Header("Cache-Control", "private, no-store")
	wire := transcriptWire{c: c, sink: c.Writer, zstd: zstd}
	if capture != nil {
		wire.sink = io.MultiWriter(c.Writer, capture)
	}
	defer func() {
		if wire.finish != nil {
			closeErr := wire.finish(err == nil)
			if err == nil {
				err = closeErr
			}
		}
	}()
	buffer := bufio.NewWriterSize(&wire, 32<<10)
	if err := write(buffer); err != nil {
		return err
	}
	return buffer.Flush()
}
