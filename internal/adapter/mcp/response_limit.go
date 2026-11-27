package mcp

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
)

const (
	maxMCPMessageBytes = 4 << 20
	maxMCPStreamBytes  = 16 << 20
)

var errMCPResponseTooLarge = errors.New("MCP server response exceeds the size limit")

// Bound bodies before the SDK reads them, including error responses and the
// remainder it drains after processing an event stream.
type responseLimitTransport struct{ http.RoundTripper }

func (t responseLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.RoundTripper.RoundTrip(req)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	limit := int64(maxMCPMessageBytes)
	mediaType, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	if mediaType == "text/event-stream" && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		limit = maxMCPStreamBytes
	}
	if resp.ContentLength > limit {
		_ = resp.Body.Close()
		return nil, errMCPResponseTooLarge
	}
	body := limitedResponseBody{http.MaxBytesReader(nil, resp.Body, limit)}
	resp.Body = body
	if req.Method == http.MethodPost && resp.StatusCode == http.StatusOK && mediaType == "application/json" {
		// A JSON reply is a single bounded message. Read it before handing it
		// to the SDK so cancellation or truncation fails this HTTP request.
		// An error in the SDK's asynchronous JSON reader closes the session.
		data, err := io.ReadAll(body)
		_ = body.Close()
		if err != nil {
			return nil, fmt.Errorf("read MCP JSON response: %w", err)
		}
		resp.Body = io.NopCloser(bytes.NewReader(data))
	}
	return resp, nil
}

type limitedResponseBody struct{ io.ReadCloser }

func (b limitedResponseBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if _, tooLarge := errors.AsType[*http.MaxBytesError](err); tooLarge {
		return n, errMCPResponseTooLarge
	}
	return n, err
}
