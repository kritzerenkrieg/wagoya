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

// fakeOR serves canned chat-completion answers in order and records requests.
func fakeOR(t *testing.T, answers ...func(w http.ResponseWriter, req map[string]any)) (*orClient, *[]map[string]any) {
	t.Helper()
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("auth header: %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("bad request json: %v", err)
		}
		reqs = append(reqs, m)
		if len(reqs) > len(answers) {
			t.Fatalf("unexpected request %d", len(reqs))
		}
		answers[len(reqs)-1](w, m)
	}))
	t.Cleanup(srv.Close)
	c := NewOpenRouter("k", "deepseek/deepseek-v4.1-flash-1017", "low").(*orClient)
	c.url = srv.URL
	return c, &reqs
}

func text(s string) func(http.ResponseWriter, map[string]any) {
	return func(w http.ResponseWriter, _ map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": s}}}})
	}
}

func TestOpenRouterToolLoop(t *testing.T) {
	call := func(w http.ResponseWriter, _ map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "search_facts", "arguments": `{"person":"P1","query":"job"}`}}}}}}})
	}
	c, reqs := fakeOR(t, call, text("You're an nurse."))
	var got []string
	out, err := c.Reply(context.Background(), "sys", "what do i do?", nil, []Tool{FactSearchTool(func(_ context.Context, p, q string, _ bool) string {
		got = append(got, p+"|"+q)
		return "P1: Job: nurse"
	})})
	if err != nil || out != "You're an nurse." {
		t.Fatalf("reply %q, %v", out, err)
	}
	if len(got) != 1 || got[0] != "P1|job" {
		t.Fatalf("search calls: %v", got)
	}
	first := (*reqs)[0]
	tools, _ := json.Marshal(first["tools"])
	if !strings.Contains(string(tools), `"search_facts"`) {
		t.Fatalf("tools missing search_facts: %s", tools)
	}
	if strings.Contains(string(tools), "openrouter:") { // W1: no provider web tools
		t.Fatalf("provider server tools sent: %s", tools)
	}
	if first["model"] != "deepseek/deepseek-v4.1-flash-1017" {
		t.Fatalf("model: %v", first["model"])
	}
	second, _ := json.Marshal((*reqs)[1]["messages"])
	if !strings.Contains(string(second), `"tool_call_id":"c1"`) || !strings.Contains(string(second), "Job: nurse") {
		t.Fatalf("tool result not sent back: %s", second)
	}
}

// W2: tool results that would push the next call past MaxCallTokens are
// withheld, and the model must answer without more tools.
func TestOpenRouterCallTokenCap(t *testing.T) {
	call := func(w http.ResponseWriter, _ map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": 90000, "completion_tokens": 200},
			"choices": []any{map[string]any{"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "big", "arguments": `{}`}}}}}}})
	}
	c, reqs := fakeOR(t, call, text("done"))
	big := Tool{Name: "big", Schema: map[string]any{"type": "object"}, Run: func(context.Context, json.RawMessage) string {
		return strings.Repeat("x", 20000) // ~8k tokens on top of 90k
	}}
	out, err := c.Reply(context.Background(), "sys", "q", nil, []Tool{big})
	if err != nil || out != "done" {
		t.Fatalf("reply %q, %v", out, err)
	}
	second := (*reqs)[1]
	msgs, _ := json.Marshal(second["messages"])
	if strings.Contains(string(msgs), "xxxx") || !strings.Contains(string(msgs), "96k-token limit") {
		t.Fatalf("big result not withheld: %.300s", msgs)
	}
	if second["tool_choice"] != "none" {
		t.Fatalf("tool_choice %v", second["tool_choice"])
	}
}

func TestOverLimit(t *testing.T) {
	if overLimit(50000, 1000, []string{strings.Repeat("x", 10000)}) {
		t.Fatal("small round flagged")
	}
	if !overLimit(95000, 500, []string{strings.Repeat("x", 2000)}) {
		t.Fatal("round past 96k not flagged")
	}
}

func TestOpenRouterImageFallback(t *testing.T) {
	reject := func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":{"message":"No endpoints found that support image input"}}`)
	}
	c, reqs := fakeOR(t, reject, text("...I can't see it."))
	out, err := c.Reply(context.Background(), "sys", "look", &Image{MediaType: "image/jpeg", Data: []byte{1, 2}}, nil)
	if err != nil || out != "...I can't see it." {
		t.Fatalf("reply %q, %v", out, err)
	}
	first, _ := json.Marshal((*reqs)[0]["messages"])
	second, _ := json.Marshal((*reqs)[1]["messages"])
	if !strings.Contains(string(first), "data:image/jpeg;base64,") {
		t.Fatalf("image not sent first: %s", first)
	}
	if strings.Contains(string(second), "image_url") || !strings.Contains(string(second), "cannot see images") {
		t.Fatalf("fallback request wrong: %s", second)
	}
}

func TestOpenRouterErrorNoRetryWithoutImage(t *testing.T) {
	c, reqs := fakeOR(t, func(w http.ResponseWriter, _ map[string]any) {
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"bad key"}}`)
	})
	if _, err := c.Reply(context.Background(), "sys", "hi", nil, nil); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("want 401 error, got %v", err)
	}
	if len(*reqs) != 1 {
		t.Fatalf("retried: %d requests", len(*reqs))
	}
}

func TestStripToolMarkup(t *testing.T) {
	if got := stripToolMarkup("Here.\n\n<｜DSML｜ calls>\n<｜DSML｜ invoke name=\"x\">"); got != "Here." {
		t.Fatalf("got %q", got)
	}
	if got := stripToolMarkup("<｜DSML｜ calls>"); !strings.HasPrefix(got, "I couldn't finish") {
		t.Fatalf("got %q", got)
	}
	if got := stripToolMarkup("plain <b>text</b>"); got != "plain <b>text</b>" {
		t.Fatalf("got %q", got)
	}
}

func TestOpenRouterReportsPromptTokens(t *testing.T) {
	c, _ := fakeOR(t, func(w http.ResponseWriter, _ map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"usage": map[string]any{"prompt_tokens": 1234}, "choices": []any{map[string]any{
			"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}})
	})
	got := 0
	ctx := WithPromptTokens(context.Background(), func(n int) { got = n })
	if _, err := c.Reply(ctx, "sys", "hi", nil, nil); err != nil || got != 1234 {
		t.Fatalf("got %d, %v", got, err)
	}
}

// A tool call written as text (DSML) is sent back for a real tool call.
func TestOpenRouterRetriesTextToolCall(t *testing.T) {
	call := func(w http.ResponseWriter, _ map[string]any) {
		json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"finish_reason": "tool_calls", "message": map[string]any{"role": "assistant", "content": "",
				"tool_calls": []any{map[string]any{"id": "c1", "type": "function",
					"function": map[string]any{"name": "search_facts", "arguments": `{"query":"x"}`}}}}}}})
	}
	c, reqs := fakeOR(t, text("scheduled\n<｜DSML｜function_calls>\n<｜DSML｜invoke name=\"search_facts\">"), call, text("done for real"))
	ran := 0
	out, err := c.Reply(context.Background(), "sys", "remind me", nil, []Tool{FactSearchTool(func(context.Context, string, string, bool) string { ran++; return "ok" })})
	if err != nil || out != "done for real" || ran != 1 {
		t.Fatalf("out %q err %v ran %d", out, err, ran)
	}
	second, _ := json.Marshal((*reqs)[1]["messages"])
	if !strings.Contains(string(second), "did not run") || strings.Contains(string(second), "DSML") {
		t.Fatalf("retry request: %s", second)
	}
}

// L4: a request's stats sum cost and tokens over its rounds and count web searches.
func TestOpenRouterStats(t *testing.T) {
	round := func(calls []any, content string, cost float64) func(http.ResponseWriter, map[string]any) {
		return func(w http.ResponseWriter, _ map[string]any) {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
				"message": map[string]any{"role": "assistant", "content": content, "tool_calls": calls}}},
				"usage": map[string]any{"cost": cost, "prompt_tokens": 1000, "completion_tokens": 50}})
		}
	}
	calls := []any{
		map[string]any{"id": "w", "type": "function", "function": map[string]any{"name": "openrouter:web_search", "arguments": `{}`}},
		map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "search_facts", "arguments": `{"query":"x"}`}},
	}
	c, _ := fakeOR(t, round(calls, "", 0.002), round(nil, "hi", 0.0005))
	var st Stats
	out, err := c.Reply(WithStats(context.Background(), &st), "sys", "news", nil,
		[]Tool{FactSearchTool(func(context.Context, string, string, bool) string { return "ok" })})
	if err != nil || out != "hi" {
		t.Fatalf("out %q err %v", out, err)
	}
	if st.Rounds != 2 || st.PromptTokens != 2000 || st.OutputTokens != 100 || st.WebSearches != 1 || st.Cost < 0.00249 || st.Cost > 0.00251 {
		t.Fatalf("stats %+v", st)
	}
}

// G5: a compatible API gets reasoning_effort and none of OpenRouter's fields.
func TestOpenAICompatRequest(t *testing.T) {
	var got map[string]any
	var hdr http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path %s", r.URL.Path)
		}
		hdr = r.Header
		json.NewDecoder(r.Body).Decode(&got)
		text("hi")(w, got)
	}))
	defer srv.Close()
	c := NewOpenAICompat(srv.URL+"/v1/", "k", "m", "none")
	out, err := c.Reply(context.Background(), "sys", "hello", nil, nil)
	if err != nil || out != "hi" {
		t.Fatalf("reply %q %v", out, err)
	}
	if got["reasoning_effort"] != "none" || got["reasoning"] != nil || got["provider"] != nil {
		t.Fatalf("request fields: %v", got)
	}
	if hdr.Get("Authorization") != "Bearer k" || hdr.Get("X-Title") != "" {
		t.Fatalf("headers: %v", hdr)
	}
}
