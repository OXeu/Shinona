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
	ZstdEncoding        = "zstd"
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

// EncodeZstdForCache only retains compressed buffers that fit within the size
// of the original body.
func EncodeZstdForCache(body []byte) []byte {
	encoder := zstdBuffers.Get().(*zstd.Encoder)
	defer zstdBuffers.Put(encoder)
	compressed := encoder.EncodeAll(body, nil)
	if len(compressed) >= len(body) {
		return nil
	}
	if cap(compressed) > len(compressed) {
		exact := make([]byte, len(compressed))
		copy(exact, compressed)
		return exact
	}
	return compressed
}

// NewZstdResponse prepares the response headers and borrows an encoder for dst.
// Finalize with commit=false to discard buffered output after a failed write.
func NewZstdResponse(response http.ResponseWriter, dst io.Writer) (*zstd.Encoder, func(commit bool) error) {
	response.Header().Del("Content-Length")
	response.Header().Set("Content-Encoding", ZstdEncoding)
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

// NegotiateZstd records the request header that selects the representation.
func NegotiateZstd(w http.ResponseWriter, r *http.Request) bool {
	w.Header().Add("Vary", "Accept-Encoding")
	return r.Method != http.MethodHead && acceptsZstd(r.Header.Values("Accept-Encoding"))
}

// Compress negotiates HTTP Zstandard for large JSON and Protobuf responses.
func Compress(c *gin.Context) {
	if !NegotiateZstd(c.Writer, c.Request) {
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
			if name != ZstdEncoding && name != "*" {
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
			if name == ZstdEncoding {
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
	finish func(bool) error
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
			w.writer, w.finish = NewZstdResponse(w.ResponseWriter, w.ResponseWriter)
			w.WriteHeaderNow()
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
	if w.finish != nil {
		_ = w.finish(true)
	}
}
