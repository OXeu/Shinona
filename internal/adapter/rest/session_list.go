package rest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"unsafe"

	"github.com/gin-gonic/gin"

	"github.com/OXeu/shinona/internal/adapter/httpserver"
	"github.com/OXeu/shinona/internal/auth"
	"github.com/OXeu/shinona/internal/responsecache"
)

const (
	sessionListHistoryLimit = 256
	sessionListMaxSnapshot  = 64 << 10
)

type sessionListScope struct {
	owner, project string
	filtered       bool
}

type sessionListKey struct {
	sessionListScope
	etag string
}

type sessionListVersion struct {
	key      sessionListKey
	revision int64
	body     sessionsResponse
	encoded  []byte
	zstd     func() []byte
}

func (v sessionListVersion) retainedBytes() int {
	size := int(unsafe.Sizeof(v)) + cap(v.encoded) + len(v.key.owner) + len(v.key.project) + len(v.key.etag)
	if len(v.encoded) >= httpserver.MinimumCompressSize {
		// Reserve space for compression, which runs only when requested.
		size += len(v.encoded)
	}
	size += cap(v.body.Projects) * int(unsafe.Sizeof(projectResponse{}))
	for _, project := range v.body.Projects {
		size += len(project.ID) + len(project.Name)
	}
	size += cap(v.body.Sessions) * int(unsafe.Sizeof(sessionSummary{}))
	for _, session := range v.body.Sessions {
		size += len(session.ID) + len(session.Title)
		if session.ProjectID != nil {
			size += int(unsafe.Sizeof(*session.ProjectID)) + len(*session.ProjectID)
		}
	}
	return size
}

func (v sessionListVersion) cacheWeight(budget int) (int, bool) {
	if len(v.encoded) > min(budget, sessionListMaxSnapshot) {
		return 0, false
	}
	size := v.retainedBytes()
	return size, size <= budget
}

// Recent versions allow any tab to resume from its own snapshot. Missing
// versions, including after restart or eviction, use a full response.
type sessionListHistory struct {
	mu       sync.Mutex
	versions responsecache.Store[sessionListKey, sessionListVersion]
	latest   map[sessionListScope]sessionListKey
	loading  map[sessionListRevision]*sessionListLoad
}

type sessionListRevision struct {
	scope    sessionListScope
	revision int64
}

type sessionListLoad struct {
	done    chan struct{}
	version sessionListVersion
	err     error
	retry   bool
}

func (h *sessionListHistory) get(key sessionListKey) (sessionsResponse, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	version, found := h.versions.Get(key)
	return version.body, found
}

func (h *sessionListHistory) currentLocked(scope sessionListScope, revision int64) (sessionListVersion, bool) {
	if key, ok := h.latest[scope]; ok {
		if version, cached := h.versions.Peek(key); cached && version.revision == revision {
			return h.versions.Get(key)
		}
	}
	return sessionListVersion{}, false
}

func (h *sessionListHistory) load(ctx context.Context, key sessionListRevision, build func() (sessionListVersion, error)) (sessionListVersion, error) {
	for {
		if err := ctx.Err(); err != nil {
			return sessionListVersion{}, err
		}
		h.mu.Lock()
		if version, found := h.currentLocked(key.scope, key.revision); found {
			h.mu.Unlock()
			return version, nil
		}
		load := h.loading[key]
		if load == nil {
			load = &sessionListLoad{done: make(chan struct{})}
			if h.loading == nil {
				h.loading = make(map[sessionListRevision]*sessionListLoad)
			}
			h.loading[key] = load
			h.mu.Unlock()
			// Build on this request's goroutine so the HTTP recovery still
			// catches a panic. A canceled leader must not fail other callers.
			finished := false
			defer func() {
				h.mu.Lock()
				defer h.mu.Unlock()
				load.retry = !finished || (load.err != nil && ctx.Err() != nil)
				delete(h.loading, key)
				if len(h.loading) == 0 {
					h.loading = nil
				}
				close(load.done)
			}()
			load.version, load.err = build()
			finished = true
			return load.version, load.err
		}
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return sessionListVersion{}, ctx.Err()
		case <-load.done:
			if err := ctx.Err(); err != nil {
				return sessionListVersion{}, err
			}
			if !load.retry {
				return load.version, load.err
			}
		}
	}
}

func (r *handler) buildSessionList(ctx context.Context, history *sessionListHistory, scope sessionListScope, cacheBytes int) (sessionListVersion, error) {
	var projectID *string
	if scope.filtered {
		projectID = &scope.project
	}
	snapshot, err := r.service.SessionListSnapshot(ctx, scope.owner, projectID)
	if err != nil {
		return sessionListVersion{}, r.apiError(err)
	}
	current := publicSessionList(snapshot)
	body, err := json.Marshal(current)
	if err != nil {
		return sessionListVersion{}, r.apiError(err)
	}
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%q/%t/%q\n", scope.owner, scope.filtered, scope.project)
	_, _ = hash.Write(body)
	// The same snapshot can be transported with different content encodings.
	tag := `W/"` + hex.EncodeToString(hash.Sum(nil)) + `"`
	version := sessionListVersion{key: sessionListKey{scope, tag}, revision: snapshot.Revision, body: current, encoded: body}
	if size, ok := version.cacheWeight(cacheBytes); ok {
		// The project value may refer to the full request query string.
		version.key.project = strings.Clone(scope.project)
		if len(body) >= httpserver.MinimumCompressSize {
			version.zstd = sync.OnceValue(func() []byte { return httpserver.EncodeZstdForCache(body) })
		}
		version = history.save(version, size)
	}
	return version, nil
}

func (h *sessionListHistory) save(version sessionListVersion, size int) sessionListVersion {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := version.key
	revision := version.revision
	if cached, ok := h.versions.Peek(key); ok {
		cached.revision = max(cached.revision, revision)
		version = cached
		size = cached.retainedBytes()
	}
	h.versions.Put(key, version, size, func(evicted sessionListKey) {
		if scope := evicted.sessionListScope; h.latest[scope] == evicted {
			delete(h.latest, scope)
		}
	})
	scope := key.sessionListScope
	latest, exists := h.versions.Peek(h.latest[scope])
	if !exists || latest.revision <= revision {
		h.latest[scope] = key
	}
	return version
}

func (r *handler) sessionList(cacheBytes int) gin.HandlerFunc {
	history := sessionListHistory{
		versions: responsecache.NewStore[sessionListKey, sessionListVersion](sessionListHistoryLimit, cacheBytes),
		latest:   make(map[sessionListScope]sessionListKey),
	}
	return func(c *gin.Context) {
		zstd := httpserver.NegotiateZstd(c.Writer, c.Request)
		var args struct {
			ProjectID *string `form:"projectId"`
			Delta     bool    `form:"delta"`
		}
		if !bind(c, &args, false) {
			return
		}
		ctx := c.Request.Context()
		scope := sessionListScope{owner: auth.CurrentUser(ctx).ID}
		// A tab can retain an old account after another tab changes the cookie.
		// Keep the response out of that account's shared browser snapshot.
		if expected := c.GetHeader("X-Shinona-Account"); expected != "" && expected != scope.owner {
			reject(c, http.StatusUnauthorized, "AUTH_REQUIRED", "Sign in required.")
			return
		}
		if args.ProjectID != nil {
			scope.filtered, scope.project = true, *args.ProjectID
		}
		revision, err := r.service.SessionListRevision(ctx, scope.owner)
		if err != nil {
			writeError(c, r.apiError(err))
			return
		}
		key := sessionListRevision{scope: scope, revision: revision}
		version, err := history.load(ctx, key, func() (sessionListVersion, error) {
			return r.buildSessionList(ctx, &history, scope, cacheBytes)
		})
		if err != nil {
			if ctx.Err() == nil {
				writeError(c, err)
			}
			return
		}
		tag := version.key.etag
		body := version.encoded
		c.Header("ETag", tag)
		c.Header("Cache-Control", "private, no-store")
		validators := c.Request.Header.Values("If-None-Match")
		unchanged := sessionListUnchanged(validators, tag)
		patched := false
		if args.Delta {
			// Only a single validator identifies a base for a patch.
			if !unchanged && len(validators) == 1 {
				baseTag := `W/` + strings.TrimPrefix(strings.TrimSpace(validators[0]), `W/`)
				if base, found := history.get(sessionListKey{scope, baseTag}); found {
					patch, err := json.Marshal(diffSessions(base, version.body))
					if err != nil {
						writeError(c, r.apiError(err))
						return
					}
					if len(patch) < len(body) {
						body = patch
						patched = true
					}
				}
			}
		}
		if unchanged {
			c.Status(http.StatusNotModified)
			return
		}
		const contentType = "application/json; charset=utf-8"
		if !zstd || len(body) < httpserver.MinimumCompressSize {
			c.Data(http.StatusOK, contentType, body)
			return
		}
		if !patched && version.zstd != nil {
			if compressed := version.zstd(); compressed != nil {
				c.Header("Content-Encoding", httpserver.ZstdEncoding)
				c.Data(http.StatusOK, contentType, compressed)
				return
			}
		}
		c.Header("Content-Type", contentType)
		writer, finish := httpserver.NewZstdResponse(c.Writer, c.Writer)
		_, err = writer.Write(body)
		if err = errors.Join(err, finish(err == nil)); err != nil {
			r.logger.Debug("write session list response", "error", err)
		}
	}
}

func sessionListUnchanged(validators []string, tag string) bool {
	tag = strings.TrimPrefix(tag, "W/")
	for _, line := range validators {
		if strings.TrimSpace(line) == "*" {
			return true
		}
		for value := strings.TrimLeft(line, " \t,"); value != ""; value = strings.TrimLeft(value, " \t,") {
			opaque, ok := strings.CutPrefix(strings.TrimPrefix(value, "W/"), `"`)
			if !ok {
				break
			}
			opaque, value, ok = strings.Cut(opaque, `"`)
			value = strings.TrimLeft(value, " \t")
			if !ok || (value != "" && value[0] != ',') {
				break
			}
			if `"`+opaque+`"` == tag {
				return true
			}
		}
	}
	return false
}

type listPatch[T any] struct {
	Upsert []T       `json:"upsert,omitempty"`
	Order  *[]string `json:"order,omitempty"`
}

type sessionsPatch struct {
	Delta struct {
		Projects *listPatch[projectResponse] `json:"projects,omitempty"`
		Sessions *listPatch[sessionSummary]  `json:"sessions,omitempty"`
	} `json:"delta"`
}

func diffSessions(base, current sessionsResponse) (patch sessionsPatch) {
	patch.Delta.Projects = diffList(base.Projects, current.Projects, func(item projectResponse) string { return item.ID },
		func(a, b projectResponse) bool { return a == b })
	patch.Delta.Sessions = diffList(base.Sessions, current.Sessions, func(item sessionSummary) string { return item.ID }, sameSessionSummary)
	return patch
}

func sameSessionSummary(a, b sessionSummary) bool {
	if (a.ProjectID == nil) != (b.ProjectID == nil) || (a.ProjectID != nil && *a.ProjectID != *b.ProjectID) {
		return false
	}
	a.ProjectID, b.ProjectID = nil, nil
	return a == b
}

// When membership or ordering changes, Order gives the new sequence of IDs.
// IDs omitted from Order have left the list. Upsert contains only changed rows.
func diffList[T any](base, current []T, id func(T) string, equal func(T, T) bool) *listPatch[T] {
	previous := make(map[string]T, len(base))
	order := make([]string, len(base))
	for i, item := range base {
		key := id(item)
		previous[key], order[i] = item, key
	}
	patch := listPatch[T]{}
	next := make([]string, len(current))
	for i, item := range current {
		key := id(item)
		next[i] = key
		if prior, ok := previous[key]; !ok || !equal(prior, item) {
			patch.Upsert = append(patch.Upsert, item)
		}
	}
	if !slices.Equal(order, next) {
		patch.Order = &next
	}
	if patch.Order == nil && len(patch.Upsert) == 0 {
		return nil
	}
	return &patch
}
