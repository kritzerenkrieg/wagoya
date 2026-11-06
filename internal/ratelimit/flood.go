package ratelimit

import (
	"sync"
	"time"
)

// FloodMsg is one message a Flood tracker saw.
type FloodMsg struct {
	At   time.Time
	Text string
}

// Flood counts messages per person (M1). When someone sends limit messages
// within window, Note returns them once for a check; the next trip needs
// limit messages newer than that.
type Flood struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	recent map[string][]FloodMsg
}

func NewFlood(limit int, window time.Duration) *Flood {
	return &Flood{limit: limit, window: window, recent: map[string][]FloodMsg{}}
}

// Note records a message by key at at. It returns the messages in the window
// when this one makes limit of them, else nil.
func (f *Flood) Note(key string, at time.Time, text string) []FloodMsg {
	f.mu.Lock()
	defer f.mu.Unlock()
	cut := at.Add(-f.window)
	for k, ms := range f.recent { // forget people who went quiet
		if len(ms) > 0 && !ms[len(ms)-1].At.After(cut) {
			delete(f.recent, k)
		}
	}
	ms := f.recent[key]
	i := 0
	for i < len(ms) && !ms[i].At.After(cut) {
		i++
	}
	ms = append(ms[i:], FloodMsg{At: at, Text: text})
	if len(ms) >= f.limit {
		delete(f.recent, key) // re-arm: the next check needs limit new messages
		return ms
	}
	f.recent[key] = ms
	return nil
}
