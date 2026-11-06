package ratelimit

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestPacerSpacesAndQueues(t *testing.T) {
	const interval = 60 * time.Millisecond
	p := NewPacer(interval)
	var mu sync.Mutex
	var times []time.Time
	var wg sync.WaitGroup
	start := time.Now()
	for range 5 { // burst of 5: none dropped, all spaced
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := p.Wait(context.Background()); err != nil {
				t.Error(err)
			}
			mu.Lock()
			times = append(times, time.Now())
			mu.Unlock()
		}()
	}
	wg.Wait()
	if len(times) != 5 {
		t.Fatalf("got %d sends, want 5", len(times))
	}
	last := times[0]
	for _, tm := range times {
		if tm.After(last) {
			last = tm
		}
	}
	if total := last.Sub(start); total < 4*interval-5*time.Millisecond {
		t.Fatalf("5 sends finished in %v, want >= %v", total, 4*interval)
	}
}

func TestPacerContextCancel(t *testing.T) {
	p := NewPacer(time.Hour)
	p.Wait(context.Background()) // takes the free slot
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx); err == nil {
		t.Fatal("expected context error while queued")
	}
}

func TestCooldown(t *testing.T) {
	c := NewCooldown(time.Minute)
	t0 := time.Unix(1000, 0)
	if !c.Allow("a", t0) {
		t.Fatal("first denial blocked")
	}
	if c.Allow("a", t0.Add(59*time.Second)) {
		t.Fatal("second denial within a minute allowed")
	}
	if !c.Allow("b", t0.Add(time.Second)) {
		t.Fatal("other chat blocked")
	}
	if !c.Allow("a", t0.Add(61*time.Second)) {
		t.Fatal("denial after cooldown blocked")
	}
}

func TestFlood(t *testing.T) {
	f := NewFlood(3, time.Minute)
	t0 := time.Unix(1000, 0)
	if f.Note("a", t0, "1") != nil || f.Note("a", t0.Add(10*time.Second), "2") != nil {
		t.Fatal("tripped early")
	}
	if f.Note("b", t0.Add(11*time.Second), "x") != nil {
		t.Fatal("people are counted separately")
	}
	got := f.Note("a", t0.Add(20*time.Second), "3")
	if len(got) != 3 || got[0].Text != "1" || got[2].Text != "3" {
		t.Fatalf("trip: %+v", got)
	}
	if f.Note("a", t0.Add(21*time.Second), "4") != nil || f.Note("a", t0.Add(22*time.Second), "5") != nil {
		t.Fatal("re-arm needs limit new messages")
	}
	if f.Note("a", t0.Add(23*time.Second), "6") == nil {
		t.Fatal("no second trip")
	}
	// Spread out: never 3 within a minute.
	for i := 0; i < 6; i++ {
		if f.Note("c", t0.Add(time.Duration(i)*35*time.Second), "s") != nil {
			t.Fatalf("tripped on slow messages at %d", i)
		}
	}
}
