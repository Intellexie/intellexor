package bus

import (
	"sync"

	"intellexor/internal/util"
)

type Event struct {
	ID         string         `json:"id"`
	Type       string         `json:"type"`
	Properties map[string]any `json:"properties"`
}

// Per-directory event hub (instance stream).
type Hub struct {
	mu     sync.RWMutex
	subs   map[chan Event]struct{}
	mirror []func(Event)
}

func (h *Hub) AddMirror(fn func(Event)) {
	h.mu.Lock()
	h.mirror = append(h.mirror, fn)
	h.mu.Unlock()
}

func NewHub() *Hub {
	return &Hub{subs: map[chan Event]struct{}{}}
}

func (h *Hub) Publish(eventType string, props map[string]any) {
	if props == nil {
		props = map[string]any{}
	}
	if _, ok := props["sessionID"]; ok {
		// sessions carry a directory too; nothing to do
	}
	ev := Event{ID: util.ID("evt"), Type: eventType, Properties: props}
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, fn := range h.mirror {
		fn(ev)
	}
	for ch := range h.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

func (h *Hub) Subscribe() chan Event {
	ch := make(chan Event, 512)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *Hub) Unsubscribe(ch chan Event) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
	close(ch)
}
