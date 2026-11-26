package run

import (
	"context"
	"slices"
	"sync"
)

const maxPendingSteering = 16

type SteeringMessage struct {
	ID        string `json:"id,omitempty"`
	Text      string `json:"text"`
	Delivered bool   `json:"delivered"`
}

// Steering holds messages in arrival order and seals when the agent has taken
// its last batch. Transfers reserve their position until the queue write ends.
type Steering struct {
	mu         sync.Mutex
	messages   []*steeringEntry
	next       int
	sealed     bool
	onDelivery func()
}

type steeringEntry struct {
	message   SteeringMessage
	receiving bool
	moving    chan struct{}
}

func (s *Steering) enqueue(load func() (SteeringMessage, error)) error {
	s.mu.Lock()
	if s.sealed {
		s.mu.Unlock()
		return ErrNotRunning
	}
	if len(s.messages)-s.next >= maxPendingSteering {
		s.mu.Unlock()
		return ErrQueueFull
	}
	entry := &steeringEntry{receiving: true, moving: make(chan struct{})}
	s.messages = append(s.messages, entry)
	s.mu.Unlock()

	committed := false
	var message SteeringMessage
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if committed {
			entry.message = message
			entry.receiving = false
		} else {
			s.remove(entry)
		}
		close(entry.moving)
		entry.moving = nil
	}()
	message, err := load()
	committed = err == nil
	return err
}

func (s *Steering) take(id string, use func(SteeringMessage) error) error {
	s.mu.Lock()
	if s.sealed {
		s.mu.Unlock()
		return ErrNotRunning
	}
	index := slices.IndexFunc(s.messages[s.next:], func(entry *steeringEntry) bool {
		return !entry.receiving && entry.message.ID == id
	})
	if index < 0 || s.messages[s.next+index].moving != nil {
		s.mu.Unlock()
		return ErrNotRunning
	}
	entry := s.messages[s.next+index]
	message := entry.message
	entry.moving = make(chan struct{})
	s.mu.Unlock()

	committed := false
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if committed {
			s.remove(entry)
		}
		close(entry.moving)
		entry.moving = nil
	}()
	err := use(message)
	committed = err == nil
	return err
}

func (s *Steering) remove(entry *steeringEntry) {
	// Drain stops at the first move, so reserved entries are still undelivered.
	index := s.next + slices.Index(s.messages[s.next:], entry)
	s.messages = slices.Delete(s.messages, index, index+1)
}

func (s *Steering) Drain() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	var messages []string
	for _, entry := range s.messages[s.next:] {
		if entry.moving != nil {
			break
		}
		entry.message.Delivered = true
		messages = append(messages, entry.message.Text)
	}
	s.next += len(messages)
	s.mu.Unlock()
	if len(messages) > 0 && s.onDelivery != nil {
		s.onDelivery()
	}
	return messages
}

func (s *Steering) Finish(ctx context.Context) []string {
	if s == nil {
		return nil
	}
	for ctx.Err() == nil {
		if messages := s.Drain(); len(messages) > 0 {
			return messages
		}
		s.mu.Lock()
		if s.next == len(s.messages) {
			s.sealed = true
			s.mu.Unlock()
			return nil
		}
		moving := s.messages[s.next].moving
		s.mu.Unlock()
		if moving != nil {
			select {
			case <-moving:
			case <-ctx.Done():
				return nil
			}
		}
	}
	return nil
}

func (s *Steering) close() []SteeringMessage {
	s.mu.Lock()
	s.sealed = true
	var moving []<-chan struct{}
	for _, entry := range s.messages[s.next:] {
		if entry.moving != nil {
			moving = append(moving, entry.moving)
		}
	}
	s.mu.Unlock()
	for _, done := range moving {
		<-done
	}
	return s.snapshot()
}

func (s *Steering) snapshot() []SteeringMessage {
	s.mu.Lock()
	defer s.mu.Unlock()
	messages := slices.Grow([]SteeringMessage(nil), len(s.messages))
	for _, entry := range s.messages {
		if !entry.receiving {
			messages = append(messages, entry.message)
		}
	}
	return messages
}
