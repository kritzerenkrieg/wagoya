package tools

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeResearcher stands in for llm.Researcher (W8).
type fakeResearcher struct {
	digestErr, searchErr error
	sources, task        string
	searchCalls          int
}

func (f *fakeResearcher) Digest(_ context.Context, task, sources string, maxTokens int) (string, error) {
	f.task, f.sources = task, sources
	return "Price: Rp 1.234.000 [2]\nNot found: nothing", f.digestErr
}

func (f *fakeResearcher) SearchDigest(_ context.Context, task string, maxTokens, rawTokens int) (string, error) {
	f.task = task
	f.searchCalls++
	return "Price: Rp 999 [1]\nSources:\n[1] Shop - https://shop.example/1", f.searchErr
}

var fixedNow = func() time.Time { return time.Date(2026, 11, 19, 12, 0, 0, 0, time.UTC) }

func TestDeepSearchBuiltin(t *testing.T) {
	srv, _ := fakeDaemon(t, strings.Repeat("A long page about RAM prices. ", 20000))
	f := &fakeResearcher{}
	w := NewWeb(WebConfig{BaseURL: srv.URL, Researcher: f, RawTokens: 20000, DigestTokens: 8000, Now: fixedNow})
	out := w.Search(context.Background(), "harga ram", "how much is 16GB DDR5?")
	if !strings.Contains(out, "Rp 1.234.000 [2]") || !strings.Contains(out, "[5] Result 5 - https://example.com/5") {
		t.Fatalf("digest output: %s", out)
	}
	if len(f.sources) > int(20000*rawBytesPerToken) {
		t.Fatalf("sources %d bytes, over the raw budget", len(f.sources))
	}
	if !strings.Contains(f.sources, "[1] Result 1\nhttps://example.com/1\nA long page") {
		t.Fatalf("sources: %.200s", f.sources)
	}
	if f.task != "Today: 2026-11-19\nSearch: harga ram\nQuestion: how much is 16GB DDR5?" {
		t.Fatalf("task: %q", f.task)
	}
}

func TestDeepSearchFallbacks(t *testing.T) {
	srv, _ := fakeDaemon(t, "page")
	// A failed digest returns the W1 snippets.
	w := NewWeb(WebConfig{BaseURL: srv.URL, Researcher: &fakeResearcher{digestErr: errors.New("boom")}, DigestTokens: 8000})
	if out := w.Search(context.Background(), "q", ""); !strings.HasPrefix(out, "1. Result 1") {
		t.Fatalf("digest failure: %s", out)
	}
	// A failed openrouter search falls back to builtin.
	f := &fakeResearcher{searchErr: errors.New("402")}
	w = NewWeb(WebConfig{BaseURL: srv.URL, Researcher: f, Backend: "openrouter", DigestTokens: 8000})
	if w.Backend() != BackendOpenRouter {
		t.Fatal("backend not openrouter")
	}
	if out := w.Search(context.Background(), "q", ""); f.searchCalls != 1 || !strings.Contains(out, "Rp 1.234.000") {
		t.Fatalf("openrouter failure: %s", out)
	}
	// A working openrouter search is used as is.
	f.searchErr = nil
	if out := w.Search(context.Background(), "q", ""); !strings.Contains(out, "Rp 999 [1]") {
		t.Fatalf("openrouter: %s", out)
	}
	// No OpenRouter key: snippets only, and openrouter is refused.
	w = NewWeb(WebConfig{BaseURL: srv.URL, Backend: "openrouter", DigestTokens: 8000})
	if w.Backend() != BackendBuiltin {
		t.Fatal("unkeyed openrouter not replaced by builtin")
	}
	if err := w.SetBackend(BackendOpenRouter); !errors.Is(err, ErrNoKey) || w.Backend() != BackendBuiltin {
		t.Fatalf("SetBackend without key: %v, %s", err, w.Backend())
	}
	if out := w.Search(context.Background(), "q", ""); !strings.HasPrefix(out, "1. Result 1") {
		t.Fatalf("no key: %s", out)
	}
	if err := w.SetBackend("deepseek"); err == nil {
		t.Fatal("unknown backend accepted")
	}
}

func TestClampRawTokens(t *testing.T) {
	for in, want := range map[int]int{0: 64000, 64000: 64000, 100000: 72000, 72001: 72000, 100: 8000} {
		if got := ClampRawTokens(in); got != want {
			t.Errorf("ClampRawTokens(%d) = %d, want %d", in, got, want)
		}
	}
	w := NewWeb(WebConfig{Researcher: &fakeResearcher{}, DigestTokens: 99999})
	if _, d := w.Limits(); d != MaxDigestTokens {
		t.Errorf("digest %d, want %d", d, MaxDigestTokens)
	}
}
