package tools

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
)

// TestDeepSearchLive runs one real deep search per backend (W8): paid
// OpenRouter calls, about a cent each. Run with WEBSEARCH_LIVE=<daemon URL>
// and OPENROUTER_API_KEY set.
func TestDeepSearchLive(t *testing.T) {
	base, key := os.Getenv("WEBSEARCH_LIVE"), os.Getenv("OPENROUTER_API_KEY")
	if base == "" || key == "" {
		t.Skip("set WEBSEARCH_LIVE and OPENROUTER_API_KEY")
	}
	res := llm.NewResearcher(key, "deepseek/deepseek-v4.1-flash-1017")
	for _, backend := range Backends {
		w := NewWeb(WebConfig{BaseURL: base, Researcher: res, Backend: backend, DigestTokens: DefaultDigestTokens})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		st := &llm.Stats{}
		start := time.Now()
		out := w.Search(llm.WithStats(ctx, st), "harga RAM DDR5 16GB", "How much is a 16GB DDR5 RAM kit in Indonesia now, in rupiah?")
		cancel()
		t.Logf("%s: %s, $%.5f, %d searches, %d chars:\n%s", backend, time.Since(start).Round(time.Second), st.Cost, st.WebSearches, len(out), out)
		if st.Cost == 0 {
			t.Errorf("%s: no digest call was made", backend)
		}
	}
}
