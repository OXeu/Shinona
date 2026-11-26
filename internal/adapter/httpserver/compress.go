package httpserver

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/gzip"
)

const minimumGzipSize = 1024

var gzipWriters = sync.Pool{New: func() any {
	writer, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return writer
}}

// CompressJSON negotiates HTTP gzip for large JSON responses.
func CompressJSON(c *gin.Context) {
	c.Writer.Header().Add("Vary", "Accept-Encoding")
	if c.Request.Method == http.MethodHead || !acceptsGzip(c.Request.Header.Values("Accept-Encoding")) {
		return
	}
	writer := &gzipResponseWriter{ResponseWriter: c.Writer}
	c.Writer = writer
	defer writer.close()
	c.Next()
}

func acceptsGzip(values []string) bool {
	var wildcard bool
	for _, value := range values {
		for token := range strings.SplitSeq(value, ",") {
			name, parameters, _ := strings.Cut(token, ";")
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "gzip" && name != "*" {
				continue
			}
			accepted := true
			for parameter := range strings.SplitSeq(parameters, ";") {
				key, value, _ := strings.Cut(parameter, "=")
				if strings.EqualFold(strings.TrimSpace(key), "q") {
					q, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
					accepted = err == nil && q > 0 && q <= 1
				}
			}
			if name == "gzip" {
				return accepted
			}
			wildcard = accepted
		}
	}
	return wildcard
}

type gzipResponseWriter struct {
	gin.ResponseWriter
	writer *gzip.Writer
}

func (w *gzipResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *gzipResponseWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *gzipResponseWriter) Write(data []byte) (int, error) {
	if w.writer == nil && !w.Written() && len(data) >= minimumGzipSize &&
		w.Status() != http.StatusPartialContent && w.Status() < http.StatusBadRequest &&
		strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") &&
		w.Header().Get("Content-Encoding") == "" {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.writer = gzipWriters.Get().(*gzip.Writer)
		w.writer.Reset(w.ResponseWriter)
	}
	if w.writer != nil {
		return w.writer.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *gzipResponseWriter) Flush() {
	if w.writer != nil {
		_ = w.writer.Flush()
	}
	w.ResponseWriter.Flush()
}

func (w *gzipResponseWriter) close() {
	if w.writer != nil {
		_ = w.writer.Close()
		w.writer.Reset(io.Discard)
		gzipWriters.Put(w.writer)
	}
}
