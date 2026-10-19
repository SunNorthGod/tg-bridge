package pipeline

import (
	"sync"

	"tgbridge/internal/store"
)

// Event is the SSE payload pushed to browsers. One stream carries all
// chats; the client filters by the currently open conversation.
type Event struct {
	Type    string      `json:"type"` // "message" | "chats"
	ChatID  int64       `json:"chat_id,omitempty"`
	Message *MsgPayload `json:"message,omitempty"`
}

type MsgPayload struct {
	ID        int64  `json:"id"`
	ChatID    int64  `json:"chat_id"`
	Direction string `json:"direction"` // "in" | "out"
	Source    string `json:"source_text"`
	Target    string `json:"target_text"`
	Status    string `json:"status"` // done | failed | skipped
	Lang      string `json:"lang_detected,omitempty"`
	CreatedAt string `json:"created_at"`
}

// PayloadFromMessage converts a stored row into its wire shape.
func PayloadFromMessage(m *store.Message) MsgPayload {
	return MsgPayload{
		ID:        m.ID,
		ChatID:    m.ChatID,
		Direction: m.Direction,
		Source:    m.SourceText,
		Target:    m.TargetText,
		Status:    m.Status,
		Lang:      m.LangDetected,
		CreatedAt: m.CreatedAt,
	}
}

// Bus fans events out to SSE subscribers. A slow subscriber drops
// events instead of blocking workers — the client re-fetches lists on
// reconnect, so loss is recoverable.
type Bus struct {
	mu   sync.RWMutex
	subs map[chan Event]struct{}
}

func NewBus() *Bus {
	return &Bus{subs: make(map[chan Event]struct{})}
}

// Subscribe returns a buffered channel and an unsubscribe func.
func (b *Bus) Subscribe() (chan Event, func()) {
	ch := make(chan Event, 256)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		delete(b.subs, ch)
		b.mu.Unlock()
		close(ch)
	}
}

func (b *Bus) Publish(ev Event) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for ch := range b.subs {
		select {
		case ch <- ev:
		default: // drop for slow consumer
		}
	}
}
