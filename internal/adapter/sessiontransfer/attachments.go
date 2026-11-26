package sessiontransfer

import (
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const AttachmentPath = "/api/v1/sessions/{id}/turns/{turnId}/attachments/{index}"

type AttachmentReader interface {
	Attachment(ctx context.Context, ownerID, sessionID, turnID string, index int) (*run.Image, error)
}

func Attachment(reader AttachmentReader, logger *slog.Logger) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index, err := strconv.Atoi(r.PathValue("index"))
		if err != nil || index < 0 || index >= run.MaxImages {
			http.NotFound(w, r)
			return
		}
		load := timing.Start(r.Context(), "attachment_load")
		image, err := reader.Attachment(r.Context(), auth.CurrentUser(r.Context()).ID,
			r.PathValue("id"), r.PathValue("turnId"), index)
		load()
		if err != nil {
			logger.Error("read turn attachment", "error", err)
			http.Error(w, "Attachment is unavailable.", http.StatusInternalServerError)
			return
		}
		decode := timing.Start(r.Context(), "attachment_decode")
		data := imageBytes(image)
		decode()
		if len(data) == 0 {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", image.MediaType)
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		_, _ = w.Write(data)
	})
}

func imageBytes(image *run.Image) []byte {
	if image == nil || len(image.Data) > base64.StdEncoding.EncodedLen(run.MaxImageBytes) {
		return nil
	}
	switch image.MediaType {
	case "image/png", "image/jpeg", "image/webp":
	default:
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(image.Data)
	if err != nil || len(data) > run.MaxImageBytes || http.DetectContentType(data) != image.MediaType {
		return nil
	}
	return data
}
