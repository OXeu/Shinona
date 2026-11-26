package rest

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"

	"github.com/gin-gonic/gin"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/proto"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer"
	"github.com/OXeu/shinona/internal/adapter/sessiontransfer/sessionpb"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const (
	// Field 2 of TranscriptExportFrame holds a Turn.
	exportTurnField = 2
	// Reserve space for the length-prefixed terminal frame even at the byte limit.
	exportEndReserve = 16
)

func (r *handler) fullTranscriptResponse(c *gin.Context, key transcriptPageKey, settings run.Settings) error {
	ctx, finish := httpserver.StreamTiming(c.Writer, c.Request.Context())
	defer finish()
	defer timing.Start(ctx, "transcript_write")()

	var capture *boundedCapture
	err := r.service.StreamTranscript(ctx, key.owner, key.session, func(stream run.TranscriptStream) error {
		if r.transcriptPages != nil && stream.Session != nil && !stream.Session.Active {
			capture = &boundedCapture{}
		}
		return r.writeTranscriptExport(ctx, c, key.zstd, capture, stream, settings, maxFullTranscriptBytes)
	})
	if err == nil {
		r.saveTranscriptResponse(c, key, capture)
	}
	return err
}

func (r *handler) writeTranscriptExport(ctx context.Context, c *gin.Context, zstd bool, capture *boundedCapture,
	stream run.TranscriptStream, settings run.Settings, byteLimit int,
) error {
	var exportErr error
	err := writeTranscriptResponse(c, sessiontransfer.TranscriptExportMediaType, zstd, capture, func(buffer *bufio.Writer) error {
		writer := transcriptExportWriter{buffer: buffer, remaining: byteLimit - exportEndReserve}
		exportErr = r.writeExportFrames(ctx, &writer, stream, settings)
		if exportErr != nil && !c.Writer.Written() {
			return exportErr
		}

		// Once a response starts, deliver errors in a terminal record. A socket
		// failure prevents writing that record and leaves the stream incomplete.
		end := &sessionpb.TranscriptExportFrame{Item: &sessionpb.TranscriptExportFrame_End{End: &sessionpb.TranscriptExportEnd{
			Failure: exportFailure(exportErr),
		}}}
		writer.remaining = exportEndReserve
		return writer.writeFrame(end)
	})
	if err != nil {
		return err
	}
	return exportErr
}

func (r *handler) writeExportFrames(ctx context.Context, writer *transcriptExportWriter, stream run.TranscriptStream, settings run.Settings) error {
	header := &sessionpb.TranscriptExportHeader{
		SandboxSettings: sessiontransfer.SandboxSettings(settings),
		LastOptions:     sessiontransfer.RunOptions(stream.LastOptions),
		TurnCount:       int32(stream.TurnCount),
	}
	if stream.Session != nil {
		header.Session = sessiontransfer.Summary(*stream.Session)
	}
	if err := writer.writeFrame(&sessionpb.TranscriptExportFrame{Item: &sessionpb.TranscriptExportFrame_Header{Header: header}}); err != nil {
		return err
	}
	for stream.Next != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
		load := timing.Start(ctx, "transcript_load_batch")
		batch, err := stream.Next()
		load()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		if batch == nil {
			return ctx.Err()
		}
		for _, turn := range batch {
			if err := ctx.Err(); err != nil {
				return err
			}
			body, err := r.transcriptTurns.encode(ctx, turn)
			if err != nil {
				return err
			}
			if err := writer.writeTurn(body); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func exportFailure(err error) sessionpb.TranscriptExportEnd_Failure {
	switch {
	case err == nil:
		return sessionpb.TranscriptExportEnd_NONE
	case errors.Is(err, run.ErrTranscriptExportLimit):
		return sessionpb.TranscriptExportEnd_LIMIT
	case errors.Is(err, context.DeadlineExceeded):
		return sessionpb.TranscriptExportEnd_TIMEOUT
	default:
		return sessionpb.TranscriptExportEnd_INTERNAL
	}
}

type transcriptExportWriter struct {
	buffer    *bufio.Writer
	remaining int
}

func (w *transcriptExportWriter) writeFrame(frame *sessionpb.TranscriptExportFrame) error {
	body, err := proto.Marshal(frame)
	if err != nil {
		return err
	}
	var prefix [binary.MaxVarintLen64]byte
	return w.write(protowire.AppendVarint(prefix[:0], uint64(len(body))), body)
}

func (w *transcriptExportWriter) writeTurn(body []byte) error {
	// Reuse the encoded Turn without allocating a copy inside the envelope.
	frameSize := protowire.SizeTag(exportTurnField) + protowire.SizeBytes(len(body))
	var prefix [2*binary.MaxVarintLen64 + 1]byte
	header := protowire.AppendVarint(prefix[:0], uint64(frameSize))
	header = protowire.AppendTag(header, exportTurnField, protowire.BytesType)
	return w.write(protowire.AppendVarint(header, uint64(len(body))), body)
}

func (w *transcriptExportWriter) write(prefix, payload []byte) error {
	size := len(prefix) + len(payload)
	if size > w.remaining {
		return run.ErrTranscriptExportLimit
	}
	if _, err := w.buffer.Write(prefix); err != nil {
		return err
	}
	if _, err := w.buffer.Write(payload); err != nil {
		return err
	}
	w.remaining -= size
	return nil
}
