package core

import "sync"

type EventType string

const (
	EventPlayer   EventType = "player"
	EventPlaylist EventType = "playlist"
	EventJob      EventType = "job"
	EventSettings EventType = "settings"
	EventLog      EventType = "log"
	EventNetwork  EventType = "network" // Wi-Fi saved, deleted, (dis)connected
)

type Event struct {
	Type  EventType
	JobID int64 // set for EventJob
}

// Hub fans out change notifications to subscribers (web SSE clients, bot).
// Slow subscribers miss events rather than block the publisher.
type Hub struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

func NewHub() *Hub { return &Hub{subs: map[chan Event]struct{}{}} }

func (h *Hub) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs, ch)
		h.mu.Unlock()
	}
}

func (h *Hub) Publish(e Event) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
