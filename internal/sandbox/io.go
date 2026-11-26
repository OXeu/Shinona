package sandbox

import (
	"bytes"
	"errors"
	"path"
	"strings"
)

const (
	MaxFileBytes       = 64 << 10
	DefaultOutputLimit = 16 << 10
)

func WorkspacePath(name string) (string, error) {
	clean := path.Clean(name)
	if clean == "." || clean == ".." || path.IsAbs(clean) || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0) {
		return "", errors.New("use a file path relative to the workspace")
	}
	return clean, nil
}

// Output consumes a full stream while retaining at most limit bytes.
type Output struct {
	data      bytes.Buffer
	remaining int
	truncated bool
}

func NewOutput(limit int) *Output { return &Output{remaining: max(limit, 0)} }

func (o *Output) Write(data []byte) (int, error) {
	n := min(o.remaining, len(data))
	o.remaining -= n
	o.truncated = o.truncated || n < len(data)
	_, _ = o.data.Write(data[:n])
	return len(data), nil
}

func (o *Output) String() string  { return o.data.String() }
func (o *Output) Truncated() bool { return o.truncated }
