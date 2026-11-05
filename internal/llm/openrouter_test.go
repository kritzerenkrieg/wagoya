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
	c := NewOpenRouter("k", "deepseek/deepseek-v4.1-flash-1017", "low", true).(*orClient)
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
	c, reqs := fakeOR(t, call, text("You're an IoT engineer."))
	var got []string
	out, err := c.Reply(context.Background(), "sys", "what do i do?", nil, []Tool{FactSearchTool(func(_ context.Context, p, q string) string {
		got = append(got, p+"|"+q)
		return "P1: Job: IoT engineer"
	})})
	if err != nil || out != "You're an IoT engineer." {
		t.Fatalf("reply %q, %v", out, err)
	}
	if len(got) != 1 || got[0] != "P1|job" {
		t.Fatalf("search calls: %v", got)
	}
	first := (*reqs)[0]
	tools, _ := json.Marshal(first["tools"])
	for _, want := range []string{`"search_facts"`, `"openrouter:web_search"`, `"openrouter:web_fetch"`, `"max_total_results":15`} {
		if !strings.Contains(string(tools), want) {
			t.Fatalf("tools missing %s: %s", want, tools)
		}
	}
	if first["model"] != "deepseek/deepseek-v4.1-flash-1017" {
		t.Fatalf("model: %v", first["model"])
	}
	second, _ := json.Marshal((*reqs)[1]["messages"])
	if !strings.Contains(string(second), `"tool_call_id":"c1"`) || !strings.Contains(string(second), "Job: IoT engineer") {
		t.Fatalf("tool result not sent back: %s", second)
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
