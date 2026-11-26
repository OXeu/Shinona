package rest

import (
	"bufio"
	"bytes"
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

// Field 5 of sessionpb.TranscriptPage holds the embedded Turn messages.
const pageTurnField = 5

type transcriptPageKey struct {
	owner, session, policies string
	selected                 sandbox.Policy
	before                   int
	head, summary            string
	zstd, full               bool
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

func (key transcriptPageKey) withState(state run.TranscriptState) (transcriptPageKey, error) {
	summary, err := proto.Marshal(sessiontransfer.Summary(state.Session))
	key.head, key.summary = state.HeadID, string(summary)
	return key, err
}

func (r *handler) serveCachedTranscript(c *gin.Context, key transcriptPageKey) (bool, error) {
	if r.transcriptPages == nil {
		return false, nil
	}
	state, err := r.service.TranscriptState(c.Request.Context(), key.owner, key.session)
	if err != nil || state == nil || state.Session.Active {
		return false, err
	}
	key, err = key.withState(*state)
	if err != nil {
		return false, err
	}
	lookup := timing.Start(c.Request.Context(), "transcript_response_cache_hit")
	if hit, ok := r.transcriptPages.Get(key); ok {
		lookup()
		c.Header("Content-Encoding", hit.encoding)
		c.Header("Cache-Control", "private, no-store")
		c.Data(http.StatusOK, hit.contentType, hit.data)
		return true, nil
	}
	return false, nil
}

func (r *handler) saveTranscriptResponse(c *gin.Context, key transcriptPageKey, state run.TranscriptState, capture *boundedCapture) {
	if capture == nil || capture.overflow {
		return
	}
	// Use the state captured with these turns, even if a concurrent write
	// changed the session after the initial cache lookup.
	key, err := key.withState(state)
	if err != nil {
		r.logger.Debug("encode transcript cache key", "error", err)
		return
	}
	encoding := c.Writer.Header().Get("Content-Encoding")
	r.transcriptPages.Put(key, transcriptPageBody{c.Writer.Header().Get("Content-Type"), encoding, capture.Bytes()},
		capture.Cap()+len(key.owner)+len(key.session)+len(key.policies)+len(key.head)+len(key.summary)+len(key.selected))
}

func (r *handler) pagedTranscriptResponse(c *gin.Context, key transcriptPageKey, settings run.Settings) error {
	ctx := c.Request.Context()
	load := timing.Start(ctx, "transcript_load")
	page, err := r.service.TranscriptPage(ctx, key.owner, key.session, key.before)
	load()
	if err != nil {
		return err
	}
	meta := &sessionpb.TranscriptPage{SandboxSettings: sessiontransfer.SandboxSettings(settings), Complete: true}
	var capture *boundedCapture
	var turns [][]byte
	if page != nil {
		meta.Session = sessiontransfer.Summary(page.Session)
		meta.LastOptions = sessiontransfer.RunOptions(page.LastOptions())
		meta.NextBeforeSequence = int32(page.NextBeforeSequence)
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
		if r.transcriptPages != nil && !page.Session.Active {
			capture = &boundedCapture{}
		}
	}
	header, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	size := len(header)
	for _, turn := range turns {
		size += protowire.SizeTag(pageTurnField) + protowire.SizeBytes(len(turn))
	}
	compress := key.zstd && size >= httpserver.MinimumCompressSize
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
		return nil
	})
	if err == nil && capture != nil {
		r.saveTranscriptResponse(c, key, page.TranscriptState, capture)
	}
	return err
}

type boundedCapture struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedCapture) Write(data []byte) (int, error) {
	if len(data) > maxCachedTranscriptResponse-b.Len() {
		b.overflow = true
		b.Buffer = bytes.Buffer{}
	}
	if b.overflow {
		return len(data), nil
	}
	return b.Buffer.Write(data)
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
