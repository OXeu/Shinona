package run

import (
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/OXeu/shinona/internal/imagedata"
	"github.com/OXeu/shinona/internal/mcpconfig"
)

const (
	MaxImageBytes = 4 << 20
	MaxImages     = 4
	ReadOnly      = "READ_ONLY"
	Workspace     = "WORKSPACE"
)

var (
	ErrImages     = errors.New("attach up to four PNG, JPEG, or WebP images totaling at most 4 MiB")
	ErrOptions    = errors.New("invalid composer options")
	ErrNotRunning = errors.New("the run is no longer accepting this action")
	ErrQueueFull  = errors.New("this session's prompt queue is full")
)

type Image struct {
	Name      string `json:"name"`
	MediaType string `json:"mediaType"`
	Data      string `json:"data,omitempty"`
}

type Options struct {
	Model      string
	Permission string
	Thinking   string
	MCP        string   `json:",omitempty"` // Read selections saved by older versions.
	MCPServers []string `json:",omitempty"`
	ArrowJS    bool     `json:",omitempty"`
}

func (o Options) SelectedMCPServers() []string {
	if o.MCPServers == nil && o.MCP != "" {
		return []string{o.MCP}
	}
	return o.MCPServers
}

type ComposerDefaults struct {
	Model      string `json:"model"`
	Permission string `json:"permission"`
	Thinking   string `json:"thinking"`
}

type Payload struct {
	Images  []Image `json:"images,omitempty"`
	Options Options `json:"options"`
}

type RetryOptions struct {
	Selection *Options
	Model     string // Model-only override for clients that omit Selection.
}

func (o RetryOptions) Apply(previous Options) Options {
	if o.Selection != nil {
		return *o.Selection
	}
	if o.Model != "" {
		previous.Model = o.Model
	}
	return previous
}

type Result struct {
	Output           string
	Parts            []TurnPart
	AgentMessages    []byte // Adapter-encoded messages added after the turn's initial prompt.
	CompactedContext []byte // Optional model context through the completed turn.
	ContextTokens    int
	ContextWindow    int
	TokenUsage       *TokenUsage
}

// TurnPart is ordered display data produced by an agent during one turn.
type TurnPart struct {
	Kind            string `json:"kind"`
	Text            string `json:"text,omitempty"`
	Tool            string `json:"tool,omitempty"`
	Input           string `json:"input,omitempty"`
	Output          string `json:"output,omitempty"`
	Status          string `json:"status,omitempty"`
	EstimatedTokens int32  `json:"estimatedTokens,omitempty"`
}

func validatePrompt(prompt string, images []Image) (string, error) {
	prompt = strings.TrimSpace(prompt)
	if (prompt == "" && len(images) == 0) || utf8.RuneCountInString(prompt) > MaxPromptCharacters {
		return "", ErrInvalidPrompt
	}
	if len(images) > MaxImages {
		return "", ErrImages
	}
	total := 0
	for _, image := range images {
		if len(image.Name) == 0 || utf8.RuneCountInString(image.Name) > 180 {
			return "", ErrImages
		}
		data := imagedata.DecodeAs(image.Data, image.MediaType, MaxImageBytes-total)
		if data == nil {
			return "", ErrImages
		}
		total += len(data)
	}
	return prompt, nil
}

func validOptions(option Options) bool {
	switch option.Permission {
	case "", ReadOnly, Workspace:
	default:
		return false
	}
	switch option.Thinking {
	case "", "off", "minimal", "low", "medium", "high", "xhigh":
	default:
		return false
	}
	servers := option.SelectedMCPServers()
	if len(option.Model) > 256 || (option.Permission == ReadOnly && len(servers) > 0) {
		return false
	}
	return mcpconfig.ValidSelection(servers)
}
