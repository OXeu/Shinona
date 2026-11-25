// Package envd implements workspace operations against the envd data plane.
package envd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/OXeu/shinona/internal/sandbox"
)

const maxFileBytes = 64 * 1024

type Config struct {
	URL       string
	Token     string
	User      string
	Directory string
	Headers   http.Header
}

type Workspace struct {
	client *http.Client
	config Config
}

func New(client *http.Client, config Config) (*Workspace, error) {
	endpoint, err := url.Parse(config.URL)
	if err != nil || endpoint.Host == "" || endpoint.User != nil || endpoint.RawQuery != "" ||
		endpoint.Fragment != "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("envd requires an HTTP(S) endpoint without credentials, query, or fragment")
	}
	if !path.IsAbs(config.Directory) {
		return nil, errors.New("envd requires an absolute working directory")
	}
	config.URL = strings.TrimRight(config.URL, "/")
	config.Headers = config.Headers.Clone()
	if client == nil {
		client = http.DefaultClient
	}
	dataClient := *client
	dataClient.Timeout = 0
	return &Workspace{client: &dataClient, config: config}, nil
}

func (w *Workspace) Close(context.Context) error { return nil }

func (w *Workspace) ReadFile(ctx context.Context, name string) ([]byte, error) {
	endpoint, err := w.fileEndpoint(name)
	if err != nil {
		return nil, err
	}
	return w.request(ctx, http.MethodGet, endpoint, "", nil)
}

func (w *Workspace) WriteFile(ctx context.Context, name string, data []byte) error {
	if len(data) > maxFileBytes {
		return errors.New("file exceeds 64 KiB")
	}
	endpoint, err := w.fileEndpoint(name)
	if err != nil {
		return err
	}
	_, err = w.request(ctx, http.MethodPost, endpoint, "application/octet-stream", bytes.NewReader(data))
	return err
}

func (w *Workspace) fileEndpoint(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") {
		return "", errors.New("use a file path relative to the workspace")
	}
	query := url.Values{"path": {path.Join(w.config.Directory, clean)}}
	if w.config.User != "" {
		query.Set("username", w.config.User)
	}
	return "/files?" + query.Encode(), nil
}

func (w *Workspace) request(ctx context.Context, method, endpoint, contentType string, body io.Reader) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, w.config.URL+endpoint, body)
	if err != nil {
		return nil, err
	}
	w.setAuth(req.Header)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	response, err := w.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, fmt.Errorf("envd %s returned HTTP %d", method, response.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxFileBytes+1))
	if len(data) > maxFileBytes {
		return nil, errors.New("envd response exceeds 64 KiB")
	}
	return data, err
}

func (w *Workspace) setAuth(header http.Header) {
	for name, values := range w.config.Headers {
		for _, value := range values {
			header.Add(name, value)
		}
	}
	if w.config.Token != "" {
		header.Set("X-Access-Token", w.config.Token)
	}
	if w.config.User != "" {
		// envd uses Basic Auth to select the Unix user. The token authenticates the request.
		request := http.Request{Header: header}
		request.SetBasicAuth(w.config.User, "")
	}
}

var _ sandbox.Workspace = (*Workspace)(nil)
