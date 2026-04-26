// Package broker is a small in-process pub/sub for session events.
//
// Subscribers receive a buffered channel; if the consumer is slow and the
// buffer fills, events are dropped silently rather than blocking the
// publisher. This keeps the sender's stream goroutine moving even when an
// SSE client stalls.
package broker

import (
	"encoding/json"
	"sync"
)

type Event struct {
	SessionID string          `json:"session_id"`
	Type      string          `json:"type"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

type Broker struct {
	mu   sync.RWMutex
	subs map[string]map[*subscriber]struct{} // sessionID -> set
}

type subscriber struct {
	ch chan Event
}

func New() *Broker {
	return &Broker{subs: map[string]map[*subscriber]struct{}{}}
}

// Subscribe returns a buffered channel of events for sessionID and a cancel
// function to stop receiving and free the slot.
func (b *Broker) Subscribe(sessionID string) (<-chan Event, func()) {
	sub := &subscriber{ch: make(chan Event, 64)}
	b.mu.Lock()
	if b.subs[sessionID] == nil {
		b.subs[sessionID] = map[*subscriber]struct{}{}
	}
	b.subs[sessionID][sub] = struct{}{}
	b.mu.Unlock()

	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			if set, ok := b.subs[sessionID]; ok {
				delete(set, sub)
				if len(set) == 0 {
					delete(b.subs, sessionID)
				}
			}
			b.mu.Unlock()
			close(sub.ch)
		})
	}
	return sub.ch, cancel
}

// Publish delivers ev to every current subscriber of ev.SessionID.
func (b *Broker) Publish(ev Event) {
	b.mu.RLock()
	set := b.subs[ev.SessionID]
	chans := make([]chan Event, 0, len(set))
	for s := range set {
		chans = append(chans, s.ch)
	}
	b.mu.RUnlock()

	for _, c := range chans {
		select {
		case c <- ev:
		default:
			// Slow consumer: drop. Prefer recoverable behavior over
			// blocking the publisher and stalling the whole pipeline.
		}
	}
}
