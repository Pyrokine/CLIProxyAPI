package logging

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

const RequestTimelineGinKey = "REQUEST_TIMELINE"

type RequestTimelineEvent struct {
	Name      string
	Detail    string
	Timestamp time.Time
}

type RequestTimeline struct {
	mu     sync.Mutex
	seen   map[string]struct{}
	events []RequestTimelineEvent
}

func NewRequestTimeline() *RequestTimeline {
	return &RequestTimeline{
		seen: make(map[string]struct{}),
	}
}

func (t *RequestTimeline) Add(name string, detail string) {
	if t == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	t.add(RequestTimelineEvent{Name: name, Detail: strings.TrimSpace(detail), Timestamp: time.Now()})
}

func (t *RequestTimeline) AddOnce(name string, detail string) {
	if t == nil {
		return
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return
	}
	t.mu.Lock()
	if _, ok := t.seen[name]; ok {
		t.mu.Unlock()
		return
	}
	t.seen[name] = struct{}{}
	t.events = append(t.events, RequestTimelineEvent{Name: name, Detail: strings.TrimSpace(detail), Timestamp: time.Now()})
	t.mu.Unlock()
}

func (t *RequestTimeline) Snapshot() []RequestTimelineEvent {
	if t == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]RequestTimelineEvent(nil), t.events...)
}

func (t *RequestTimeline) add(event RequestTimelineEvent) {
	t.mu.Lock()
	t.events = append(t.events, event)
	t.mu.Unlock()
}

func WriteRequestTimeline(w io.Writer, events []RequestTimelineEvent) error {
	if len(events) == 0 {
		return nil
	}
	base := events[0].Timestamp
	prev := base
	if _, errWrite := io.WriteString(w, "=== REQUEST TIMELINE ===\n"); errWrite != nil {
		return errWrite
	}
	for _, event := range events {
		offset := event.Timestamp.Sub(base)
		delta := event.Timestamp.Sub(prev)
		line := fmt.Sprintf(
			"%s | +%.3fms | Δ%.3fms | %s",
			event.Timestamp.Format(time.RFC3339Nano),
			float64(offset.Microseconds())/1000,
			float64(delta.Microseconds())/1000,
			event.Name,
		)
		if event.Detail != "" {
			line += " | " + event.Detail
		}
		if _, errWrite := io.WriteString(w, line+"\n"); errWrite != nil {
			return errWrite
		}
		prev = event.Timestamp
	}
	_, errWrite := io.WriteString(w, "\n")
	return errWrite
}
