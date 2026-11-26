package rest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

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

// Recent versions allow any tab to resume from its own snapshot. Missing
// versions, including after restart or eviction, use a full response.
type sessionListHistory struct {
	mu       sync.Mutex
	versions responsecache.Store[sessionListKey, sessionListVersion]
	latest   map[sessionListScope]sessionListKey
}

func (h *sessionListHistory) get(key sessionListKey) (sessionsResponse, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	version, found := h.versions.Get(key)
	return version.body, found
}

func (h *sessionListHistory) current(scope sessionListScope, revision int64) (sessionListVersion, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if key, ok := h.latest[scope]; ok {
		if version, cached := h.versions.Peek(key); cached && version.revision == revision {
			return h.versions.Get(key)
		}
	}
	return sessionListVersion{}, false
}

func (h *sessionListHistory) save(version sessionListVersion) {
	h.mu.Lock()
	defer h.mu.Unlock()
	key := version.key
	revision := version.revision
	if cached, ok := h.versions.Peek(key); ok && cached.revision > version.revision {
		version = cached
	}
	h.versions.Put(key, version, 1, func(evicted sessionListKey) {
		if scope := evicted.sessionListScope; h.latest[scope] == evicted {
			delete(h.latest, scope)
		}
	})
	scope := key.sessionListScope
	latest, exists := h.versions.Peek(h.latest[scope])
	if revision >= 0 && (!exists || latest.revision <= revision) {
		h.latest[scope] = key
	}
}

func (r *handler) sessionList() gin.HandlerFunc {
	history := sessionListHistory{
		versions: responsecache.NewStore[sessionListKey, sessionListVersion](sessionListHistoryLimit, sessionListHistoryLimit),
		latest:   make(map[sessionListScope]sessionListKey),
	}
	return func(c *gin.Context) {
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
		version, found := history.current(scope, revision)
		if !found {
			current, err := r.sessions(ctx, args.ProjectID)
			if err != nil {
				writeError(c, err)
				return
			}
			body, err := json.Marshal(current)
			if err != nil {
				writeError(c, r.apiError(err))
				return
			}
			hash := sha256.New()
			_, _ = fmt.Fprintf(hash, "%q/%t/%q\n", scope.owner, scope.filtered, scope.project)
			_, _ = hash.Write(body)
			// The same snapshot can be transported with different content encodings.
			tag := `W/"` + hex.EncodeToString(hash.Sum(nil)) + `"`
			version = sessionListVersion{key: sessionListKey{scope, tag}, revision: revision, body: current, encoded: body}
			if len(body)+len(scope.owner)+len(scope.project) <= sessionListMaxSnapshot {
				if len(body) >= httpserver.MinimumCompressSize {
					version.zstd = sync.OnceValue(func() []byte { return httpserver.EncodeZstd(body) })
				}
				// The list uses two reads. Only reuse it when no relevant write
				// committed between fetching its revision and both queries.
				if after, err := r.service.SessionListRevision(ctx, scope.owner); err != nil || after != revision {
					version.revision = -1
				}
				defer history.save(version)
			}
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
		if !patched && version.zstd != nil && httpserver.AcceptsZstd(c.Request) {
			c.Header("Content-Encoding", "zstd")
			body = version.zstd()
		}
		c.Data(http.StatusOK, "application/json; charset=utf-8", body)
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
