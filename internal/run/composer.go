package run

import (
	"encoding/base64"
	"errors"
	"net/http"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
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
	Model      string
	Permission string
	Thinking   string
}

type Payload struct {
	Images  []Image `json:"images,omitempty"`
	Options Options `json:"options"`
}

type RetryOptions struct {
	Selection *Options
	Model     string // Model-only override for clients that omit Selection.
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

type SteeringMessage struct {
	Text      string
	Delivered bool
}

// Steering is the active run's mailbox. Seal rejects new messages at the final
// agent checkpoint, including when the agent stops before processing a message.
type Steering struct {
	mu         sync.Mutex
	messages   []SteeringMessage
	next       int
	sealed     bool
	onDelivery func()
}

func (s *Steering) enqueue(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sealed {
		return ErrNotRunning
	}
	if len(s.messages) >= 16 {
		return ErrQueueFull
	}
	s.messages = append(s.messages, SteeringMessage{Text: text})
	return nil
}

func (s *Steering) Drain() []string { return s.drain(false) }

func (s *Steering) Finish() []string { return s.drain(true) }

func (s *Steering) drain(final bool) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	var result []string
	for i := s.next; i < len(s.messages); i++ {
		s.messages[i].Delivered = true
		result = append(result, s.messages[i].Text)
	}
	s.next = len(s.messages)
	if final && len(result) == 0 {
		s.sealed = true
	}
	s.mu.Unlock()
	if len(result) > 0 && s.onDelivery != nil {
		s.onDelivery()
	}
	return result
}

func (s *Steering) close() []SteeringMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sealed = true
	return s.messages
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
		switch image.MediaType {
		case "image/jpeg", "image/png", "image/webp":
		default:
			return "", ErrImages
		}
		if len(image.Data) > base64.StdEncoding.EncodedLen(MaxImageBytes-total) {
			return "", ErrImages
		}
		data, err := base64.StdEncoding.DecodeString(image.Data)
		total += len(data)
		if err != nil || total > MaxImageBytes || http.DetectContentType(data) != image.MediaType {
			return "", ErrImages
		}
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
	if len(option.Model) > 256 || len(servers) > 16 || (option.Permission == ReadOnly && len(servers) > 0) {
		return false
	}
	seen := make(map[string]bool, len(servers))
	for _, name := range servers {
		if name == "" || len(name) > 80 || seen[name] {
			return false
		}
		seen[name] = true
	}
	return true
}

func (s *Steering) snapshot() []SteeringMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.messages)
}
