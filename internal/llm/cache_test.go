package llm

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// P8: every Messages API request carries a cache breakpoint on the system
// prompt and the top-level automatic one, and cache reads are counted.
func TestAPICacheBreakpoints(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"m1","type":"message","role":"assistant","model":"m","stop_reason":"end_turn",
			"content":[{"type":"text","text":"hi"}],
			"usage":{"input_tokens":10,"cache_creation_input_tokens":0,"cache_read_input_tokens":900,"output_tokens":2}}`)
	}))
	defer srv.Close()
	a := &apiClient{c: anthropic.NewClient(option.WithAPIKey("k"), option.WithBaseURL(srv.URL), option.WithMaxRetries(0)), model: "m", effort: "low"}
	var st Stats
	out, err := a.Reply(WithStats(context.Background(), &st), "system text", "prompt", nil, nil)
	if err != nil || out != "hi" {
		t.Fatalf("reply: %q %v", out, err)
	}
	if cc, _ := body["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("no top-level cache_control: %v", body["cache_control"])
	}
	sys, _ := body["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system: %v", body["system"])
	}
	if cc, _ := sys[0].(map[string]any)["cache_control"].(map[string]any); cc["type"] != "ephemeral" {
		t.Errorf("no cache_control on the system prompt: %v", sys[0])
	}
	// J6: the client reports what it spent, so the golden budget applies.
	a.model = "claude-haiku-5-5"
	if _, err := a.Reply(context.Background(), "s", "p", nil, nil); err != nil || a.Cost() <= 0 {
		t.Errorf("cost not reported: %v %v", a.Cost(), err)
	}
	if st.CachedTokens != 900 || st.PromptTokens != 910 {
		t.Errorf("stats: cached %d, prompt %d", st.CachedTokens, st.PromptTokens)
	}
}
