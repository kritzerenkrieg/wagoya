package tools

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestWebToolsLive runs real searches and page reads through an
// open-websearch daemon (W1). Run with WEBSEARCH_LIVE=http://127.0.0.1:3210.
func TestWebToolsLive(t *testing.T) {
	base := os.Getenv("WEBSEARCH_LIVE")
	if base == "" {
		t.Skip("set WEBSEARCH_LIVE to the daemon URL")
	}
	ts := WebTools(base)
	search, read := ts[0], ts[1]
	for _, q := range []string{"newest Go release version", "harga claude sonnet per million token", "jakarta weather today"} {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		out := search.Run(ctx, []byte(`{"query":`+quote(q)+`}`))
		cancel()
		t.Logf("search %q (%d chars):\n%s", q, len(out), out)
		if strings.HasPrefix(out, "search failed") || len(out) > 3000 {
			t.Errorf("search %q: %d chars: %s", q, len(out), firstN(out, 200))
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out := read.Run(ctx, []byte(`{"url":"https://go.dev/doc/devel/release"}`))
	t.Logf("read (%d chars): %s", len(out), firstN(out, 400))
	if strings.HasPrefix(out, "read failed") || len([]rune(out)) > webPageChars+500 {
		t.Errorf("read: %d chars: %s", len(out), firstN(out, 200))
	}
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

func firstN(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
