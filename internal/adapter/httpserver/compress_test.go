package httpserver

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
)

func TestCompressNegotiatesZstandardForJSON(t *testing.T) {
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderMaxWindow(8<<20))
	if err != nil {
		t.Fatal(err)
	}
	defer decoder.Close()
	large := `{"data":"` + strings.Repeat("x", minimumCompressSize) + `"}`
	for _, tt := range []struct {
		name, accept, body string
		status             int
		compressed         bool
	}{
		{"accepted", "gzip, ZSTD;q=0.5", large, http.StatusOK, true},
		{"wildcard", "*;q=1", large, http.StatusOK, true},
		{"rejected", "*;q=1, zstd;q=0", large, http.StatusOK, false},
		{"fallback", "gzip", large, http.StatusOK, false},
		{"small", "zstd", `{}`, http.StatusOK, false},
		{"partial", "zstd", large, http.StatusPartialContent, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			router := gin.New()
			router.GET("/", Compress, func(c *gin.Context) {
				c.Header("Content-Length", strconv.Itoa(len(tt.body)))
				c.Data(tt.status, "application/json; charset=utf-8", []byte(tt.body))
			})
			request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/", nil)
			request.Header.Set("Accept-Encoding", tt.accept)
			response := httptest.NewRecorder()
			router.ServeHTTP(response, request)
			if response.Code != tt.status || response.Header().Get("Vary") != "Accept-Encoding" {
				t.Fatalf("unexpected response: status %d, headers %v", response.Code, response.Header())
			}
			body := response.Body.Bytes()
			if tt.compressed {
				if response.Header().Get("Content-Encoding") != "zstd" || response.Header().Get("Content-Length") != "" {
					t.Fatalf("unexpected compressed headers: %v", response.Header())
				}
				body, err = decoder.DecodeAll(body, nil)
				if err != nil {
					t.Fatal(err)
				}
			} else if response.Header().Get("Content-Encoding") != "" {
				t.Fatalf("unexpected compression: %v", response.Header())
			}
			if string(body) != tt.body {
				t.Fatal("compression changed the JSON body")
			}
		})
	}
}
