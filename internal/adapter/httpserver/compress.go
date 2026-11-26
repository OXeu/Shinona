package httpserver

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

const (
	ProtobufMediaType   = "application/protobuf"
	MinimumCompressSize = 1024
)

func newZstdEncoder() any {
	writer, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest),
		zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(256<<10),
		zstd.WithLowerEncoderMem(true), zstd.WithEncoderCRC(false))
	if err != nil {
		panic(err)
	}
	return writer
}

var zstdWriters = sync.Pool{New: newZstdEncoder}
var zstdBuffers = sync.Pool{New: newZstdEncoder}

// EncodeZstd compresses a small, complete representation for response caches.
func EncodeZstd(body []byte) []byte {
	encoder := zstdBuffers.Get().(*zstd.Encoder)
	defer zstdBuffers.Put(encoder)
	return encoder.EncodeAll(body, nil)
}

// NewZstdStream borrows an encoder. Finalize with commit=false to discard
// buffered output after an interrupted or failed response.
func NewZstdStream(dst io.Writer) (io.Writer, func(commit bool) error) {
	writer := zstdWriters.Get().(*zstd.Encoder)
	writer.Reset(dst)
	return writer, func(commit bool) error {
		var err error
		if commit {
			err = writer.Close()
		}
		writer.Reset(io.Discard)
		zstdWriters.Put(writer)
		return err
	}
}

func AcceptsZstd(r *http.Request) bool {
	return r.Method != http.MethodHead && acceptsZstd(r.Header.Values("Accept-Encoding"))
}

// Compress negotiates HTTP Zstandard for large JSON and Protobuf responses.
func Compress(c *gin.Context) {
	c.Writer.Header().Add("Vary", "Accept-Encoding")
	if !AcceptsZstd(c.Request) {
		return
	}
	writer := &zstdResponseWriter{ResponseWriter: c.Writer}
	c.Writer = writer
	defer writer.close()
	c.Next()
}

func acceptsZstd(values []string) bool {
	var wildcard bool
	for _, value := range values {
		for token := range strings.SplitSeq(value, ",") {
			name, parameters, _ := strings.Cut(token, ";")
			name = strings.ToLower(strings.TrimSpace(name))
			if name != "zstd" && name != "*" {
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
			if name == "zstd" {
				return accepted
			}
			wildcard = accepted
		}
	}
	return wildcard
}

type zstdResponseWriter struct {
	gin.ResponseWriter
	writer *zstd.Encoder
}

func (w *zstdResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *zstdResponseWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *zstdResponseWriter) Write(data []byte) (int, error) {
	if w.writer == nil && !w.Written() && len(data) >= MinimumCompressSize &&
		w.Status() != http.StatusPartialContent && w.Status() < http.StatusBadRequest {
		header := w.Header()
		mediaType, _, _ := strings.Cut(header.Get("Content-Type"), ";")
		if (mediaType == "application/json" || mediaType == ProtobufMediaType) && header.Get("Content-Encoding") == "" {
			header.Del("Content-Length")
			header.Set("Content-Encoding", "zstd")
			w.WriteHeaderNow()
			w.writer = zstdWriters.Get().(*zstd.Encoder)
			w.writer.Reset(w.ResponseWriter)
		}
	}
	if w.writer != nil {
		return w.writer.Write(data)
	}
	return w.ResponseWriter.Write(data)
}

func (w *zstdResponseWriter) Flush() {
	if w.writer != nil {
		_ = w.writer.Flush()
	}
	w.ResponseWriter.Flush()
}

func (w *zstdResponseWriter) close() {
	if w.writer != nil {
		_ = w.writer.Close()
		w.writer.Reset(io.Discard)
		zstdWriters.Put(w.writer)
	}
}
