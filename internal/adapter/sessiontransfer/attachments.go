package sessiontransfer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/imagedata"
	"github.com/OXeu/shinona/internal/responsecache"
	"github.com/OXeu/shinona/internal/run"
	"github.com/OXeu/shinona/internal/timing"
)

const AttachmentPath = "/api/v1/sessions/{id}/turns/{turnId}/attachments/{index}"

type AttachmentReader interface {
	Attachment(ctx context.Context, ownerID, sessionID, turnID string, index int) (*run.Image, error)
	AttachmentTurnVisible(ctx context.Context, ownerID, sessionID, turnID string) (bool, error)
}

type attachmentKey struct {
	owner, session, turn string
	index                int
}

type attachmentResponse struct {
	mediaType, tag string
	data           []byte
}

func Attachment(reader AttachmentReader, logger *slog.Logger) http.Handler {
	images := responsecache.New[attachmentKey, attachmentResponse](256, 16<<20)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		w.Header().Add("Vary", "Cookie")
		index, err := strconv.Atoi(r.PathValue("index"))
		if err != nil || index < 0 || index >= run.MaxImages {
			httpserver.WriteError(w, http.StatusNotFound, "ATTACHMENT_NOT_FOUND", "Attachment not found.")
			return
		}
		key := attachmentKey{auth.CurrentUser(r.Context()).ID, r.PathValue("id"), r.PathValue("turnId"), index}
		image, found := images.Get(key)
		if found {
			// Only the cached bytes are immutable. Revert, archive, or
			// account deletion can remove access to the turn.
			var visible bool
			visible, err = reader.AttachmentTurnVisible(r.Context(), key.owner, key.session, key.turn)
			if err == nil && !visible {
				httpserver.WriteError(w, http.StatusNotFound, "ATTACHMENT_NOT_FOUND", "Attachment not found.")
				return
			}
		} else {
			image, err = loadAttachment(r.Context(), reader, key)
			if err == nil && len(image.data) != 0 {
				images.Put(key, image, cap(image.data)+len(key.owner)+len(key.session)+len(key.turn)+128)
			}
		}
		if err != nil {
			logger.Error("read turn attachment", "error", err)
			httpserver.WriteError(w, http.StatusInternalServerError, "ATTACHMENT_UNAVAILABLE", "Attachment is unavailable.")
			return
		}
		if len(image.data) == 0 {
			httpserver.WriteError(w, http.StatusNotFound, "ATTACHMENT_NOT_FOUND", "Attachment not found.")
			return
		}
		w.Header().Set("Content-Type", image.mediaType)
		w.Header().Set("ETag", image.tag)
		w.Header().Set("Cache-Control", "private, no-cache")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(image.data))
	})
}

func loadAttachment(ctx context.Context, reader AttachmentReader, key attachmentKey) (attachmentResponse, error) {
	load := timing.Start(ctx, "attachment_load")
	image, err := reader.Attachment(ctx, key.owner, key.session, key.turn, key.index)
	load()
	if err != nil {
		return attachmentResponse{}, err
	}
	defer timing.Start(ctx, "attachment_decode")()
	if image == nil {
		return attachmentResponse{}, nil
	}
	data := imagedata.DecodeAs(image.Data, image.MediaType, run.MaxImageBytes)
	if data == nil {
		return attachmentResponse{}, nil
	}
	hash := sha256.Sum256(data)
	return attachmentResponse{image.MediaType, `"` + hex.EncodeToString(hash[:]) + `"`, data}, nil
}
