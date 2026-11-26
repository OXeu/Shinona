package rest

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
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
)

type transcriptPageKey struct {
	owner, session, policies string
	selected                 sandbox.Policy
	before                   int
	revision                 int64
	zstd, full               bool
}

type transcriptPageBody struct {
	encoding string
	data     []byte
}

func (r *handler) transcriptResponseKey(c *gin.Context, owner, session string, before int, full bool, settings run.Settings) (transcriptPageKey, error) {
	key := transcriptPageKey{owner: owner, session: session, before: before, full: full,
		selected: settings.Selected, zstd: httpserver.AcceptsZstd(c.Request)}
	if r.transcriptPages == nil {
		return key, nil
	}
	var policies strings.Builder
	for _, policy := range settings.Available {
		policies.WriteString(string(policy))
		policies.WriteByte(',')
	}
	key.policies = policies.String()
	revision, err := r.service.SessionListRevision(c.Request.Context(), owner)
	key.revision = revision
	return key, err
}

func (r *handler) serveCachedTranscript(c *gin.Context, key transcriptPageKey) bool {
	if r.transcriptPages != nil {
		lookup := timing.Start(c.Request.Context(), "transcript_response_cache_hit")
		if hit, ok := r.transcriptPages.Get(key); ok {
			lookup()
			c.Header("Content-Encoding", hit.encoding)
			c.Header("Cache-Control", "private, no-store")
			c.Data(http.StatusOK, httpserver.ProtobufMediaType, hit.data)
			return true
		}
	}
	return false
}

func (r *handler) saveTranscriptResponse(c *gin.Context, key transcriptPageKey, capture *boundedCapture) {
	if capture == nil || capture.overflow {
		return
	}
	if revision, err := r.service.SessionListRevision(c.Request.Context(), key.owner); err == nil && revision == key.revision {
		encoding := c.Writer.Header().Get("Content-Encoding")
		r.transcriptPages.Put(key, transcriptPageBody{encoding, capture.Bytes()},
			capture.Cap()+len(key.owner)+len(key.session)+len(key.policies)+128)
	}
}

func (r *handler) pagedTranscriptResponse(c *gin.Context, key transcriptPageKey, settings run.Settings) error {
	ctx := c.Request.Context()
	load := timing.Start(ctx, "transcript_load")
	page, err := r.service.TranscriptPage(ctx, key.owner, key.session, key.before)
	load()
	if err != nil {
		return err
	}
	var summary *run.Session
	var options *run.Options
	var next int
	var capture *boundedCapture
	if page != nil {
		summary, options, next = &page.Session, page.LastOptions(), page.NextBeforeSequence
		if r.transcriptPages != nil && !page.Session.Active {
			capture = &boundedCapture{}
		}
	}
	err = writeTranscriptResponse(c, key.zstd, capture, 0, func(w io.Writer) error {
		if err := writeTranscriptHeader(w, summary, options, next, settings); err != nil {
			return err
		}
		if page != nil {
			return r.writeTranscriptTurns(ctx, w, page.Turns)
		}
		return nil
	})
	if err == nil {
		r.saveTranscriptResponse(c, key, capture)
	}
	return err
}

func (r *handler) fullTranscriptResponse(c *gin.Context, key transcriptPageKey, settings run.Settings) error {
	ctx, cancel := context.WithTimeout(c.Request.Context(), fullTranscriptTimeout)
	defer cancel()
	deadline, _ := ctx.Deadline()
	controller := http.NewResponseController(c.Writer)
	_ = controller.SetWriteDeadline(deadline)
	ctx, finish := httpserver.StreamTiming(c.Writer, ctx)
	defer finish()
	defer timing.Start(ctx, "transcript_write")()
	var capture *boundedCapture
	err := r.service.StreamTranscript(ctx, key.owner, key.session, func(stream run.TranscriptStream) error {
		if r.transcriptPages != nil && stream.Session != nil && !stream.Session.Active {
			capture = &boundedCapture{}
		}
		return writeTranscriptResponse(c, key.zstd, capture, maxFullTranscriptBytes, func(w io.Writer) error {
			if err := writeTranscriptHeader(w, stream.Session, stream.LastOptions, 0, settings); err != nil {
				return err
			}
			if stream.Next == nil {
				return nil
			}
			for {
				firstRead := timing.Start(ctx, "transcript_load_batch")
				batch, err := stream.Next()
				firstRead()
				if err != nil || batch == nil {
					return err
				}
				if err := r.writeTranscriptTurns(ctx, w, batch); err != nil {
					return err
				}
			}
		})
	})
	if err == nil {
		r.saveTranscriptResponse(c, key, capture)
	}
	return err
}

func writeTranscriptHeader(w io.Writer, summary *run.Session, options *run.Options, before int, settings run.Settings) error {
	meta := &sessionpb.TranscriptPage{SandboxSettings: sessiontransfer.SandboxSettings(settings)}
	if summary != nil {
		meta.Session = sessiontransfer.Summary(*summary)
		meta.NextBeforeSequence = int32(before)
		meta.LastOptions = sessiontransfer.RunOptions(options)
	}
	header, err := proto.Marshal(meta)
	if err != nil {
		return err
	}
	_, err = w.Write(header)
	return err
}

func (r *handler) writeTranscriptTurns(ctx context.Context, w io.Writer, batch []run.Run) error {
	for _, turn := range batch {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := r.transcriptTurns.encode(ctx, turn)
		if err != nil {
			return err
		}
		if _, err := w.Write(body); err != nil {
			return err
		}
	}
	return nil
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
	c       *gin.Context
	sink    io.Writer
	writer  io.Writer
	finish  func(bool) error
	zstd    bool
	limit   int
	written int
}

func (w *transcriptWire) Write(data []byte) (int, error) {
	if w.limit > 0 && len(data) > w.limit-w.written {
		return 0, run.ErrTranscriptExportLimit
	}
	if w.writer == nil {
		w.writer = w.sink
		if w.zstd && len(data) >= httpserver.MinimumCompressSize {
			w.c.Header("Content-Encoding", "zstd")
			w.writer, w.finish = httpserver.NewZstdStream(w.sink)
		}
	}
	written, err := w.writer.Write(data)
	w.written += written
	return written, err
}

func writeTranscriptResponse(c *gin.Context, zstd bool, capture *boundedCapture, limit int, write func(io.Writer) error) (err error) {
	c.Header("Content-Type", httpserver.ProtobufMediaType)
	c.Header("Cache-Control", "private, no-store")
	wire := transcriptWire{c: c, sink: c.Writer, zstd: zstd, limit: limit}
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
	end, err := proto.Marshal(&sessionpb.TranscriptPage{Complete: true})
	if err != nil {
		return err
	}
	if _, err := buffer.Write(end); err != nil {
		return err
	}
	return buffer.Flush()
}
