// Package ratelimit paces outgoing WhatsApp messages (B18) and throttles
// repeated denials per chat (B14).
package ratelimit

import (
	"context"
	"sync"
	"time"
)

// Pacer spaces events at least interval apart across all callers. Callers
// beyond the rate wait in FIFO order of their Wait calls, so overflow queues
// instead of being dropped.
type Pacer struct {
	mu       sync.Mutex
	interval time.Duration
	next     time.Time // earliest time the next slot may start
	now      func() time.Time
}

func NewPacer(interval time.Duration) *Pacer {
	return &Pacer{interval: interval, now: time.Now}
}

// Wait blocks until the caller's reserved slot. It returns ctx's error if the
// context ends first; the slot is then lost, which only adds spacing.
func (p *Pacer) Wait(ctx context.Context) error {
	p.mu.Lock()
	now := p.now()
	slot := p.next
	if slot.Before(now) {
		slot = now
	}
	p.next = slot.Add(p.interval)
	p.mu.Unlock()

	d := slot.Sub(now)
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Cooldown allows one event per key per period.
type Cooldown struct {
	mu     sync.Mutex
	period time.Duration
	last   map[string]time.Time
}

func NewCooldown(period time.Duration) *Cooldown {
	return &Cooldown{period: period, last: map[string]time.Time{}}
}

// Allow reports whether key may fire at now, and records it if so.
func (c *Cooldown) Allow(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t, ok := c.last[key]; ok && now.Sub(t) < c.period {
		return false
	}
	c.last[key] = now
	if len(c.last) > 10000 { // drop expired keys so the map can't grow forever
		for k, t := range c.last {
			if now.Sub(t) >= c.period {
				delete(c.last, k)
			}
		}
	}
	return true
}
