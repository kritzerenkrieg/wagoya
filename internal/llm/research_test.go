package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResearcher checks the W8 requests: OpenRouter's server tools with the
// cheapest engines and capped calls, and the cost added to the reply's stats
// without its rounds or tokens.
func TestResearcher(t *testing.T) {
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		bodies = append(bodies, m)
		io.WriteString(w, `{"choices":[{"message":{"content":"Fact [1]\nNot found: nothing"}}],"provider":"X",
			"usage":{"cost":0.004,"prompt_tokens":50000,"completion_tokens":900,"server_tool_use_details":{"web_search_requests":1}}}`)
	}))
	defer srv.Close()
	r := NewResearcher("k", "deepseek/x")
	r.c.url = srv.URL
	st := &Stats{Rounds: 1, PromptTokens: 100}
	ctx := WithStats(context.Background(), st)
	if out, err := r.SearchDigest(ctx, "Search: ram", 8000, 64000); err != nil || !strings.HasPrefix(out, "Fact [1]") {
		t.Fatalf("SearchDigest: %q %v", out, err)
	}
	if out, err := r.Digest(ctx, "Search: ram", "[1] page", 8000); err != nil || out == "" {
		t.Fatalf("Digest: %q %v", out, err)
	}
	if st.Cost != 0.008 || st.Rounds != 1 || st.PromptTokens != 100 || st.WebSearches != 2 {
		t.Fatalf("stats: %+v", st)
	}
	tools := bodies[0]["tools"].([]any)
	search, fetch := tools[0].(map[string]any), tools[1].(map[string]any)
	sp, fp := search["parameters"].(map[string]any), fetch["parameters"].(map[string]any)
	if search["type"] != "openrouter:web_search" || sp["engine"] != "parallel" || sp["mode"] != "fast" ||
		fetch["type"] != "openrouter:web_fetch" || fp["engine"] != "openrouter" {
		t.Fatalf("tools: %v", tools)
	}
	calls, pages := SearchToolCalls(64000)
	if bodies[0]["max_tool_calls"] != float64(calls) || fp["max_uses"] != float64(pages) || calls*orPageTokens > 72000 {
		t.Fatalf("caps: calls %v uses %v", bodies[0]["max_tool_calls"], fp["max_uses"])
	}
	if bodies[1]["tools"] != nil || bodies[1]["max_tokens"] != float64(8000+digestThinking) {
		t.Fatalf("digest request: %v", bodies[1])
	}
}
