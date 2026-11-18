package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDaemon serves open-websearch's /search and /fetch-web envelopes (W1).
func fakeDaemon(t *testing.T, page string) (*httptest.Server, *[]map[string]any) {
	t.Helper()
	var reqs []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		json.Unmarshal(b, &m)
		m["path"] = r.URL.Path
		reqs = append(reqs, m)
		switch r.URL.Path {
		case "/search":
			if m["query"] == "blocked" {
				fmt.Fprint(w, `{"status":"error","data":null,"error":{"code":"engine_failed","message":"duckduckgo blocked"},"hint":null}`)
				return
			}
			var res []map[string]any
			for i := range 8 {
				res = append(res, map[string]any{"title": fmt.Sprintf("<b>Result</b> %d", i+1), "url": fmt.Sprintf("https://example.com/%d", i+1),
					"description": "Today&#x27;s <b>weather</b> " + strings.Repeat("long ", 200)})
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": map[string]any{"results": res}})
		case "/fetch-web":
			want := int(m["maxChars"].(float64))
			content, trunc := page, false
			if len([]rune(content)) > want {
				content, trunc = string([]rune(content)[:want]), true
			}
			json.NewEncoder(w).Encode(map[string]any{"status": "ok", "data": map[string]any{
				"finalUrl": "https://example.com/page", "title": "Example", "truncated": trunc, "content": content}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &reqs
}

func TestWebSearch(t *testing.T) {
	srv, reqs := fakeDaemon(t, "")
	search := WebTools(srv.URL)[0]
	out := search.Run(context.Background(), json.RawMessage(`{"query":"jakarta weather"}`))
	if strings.Count(out, "https://example.com/") != webSearchResults || strings.Contains(out, "<b>") || !strings.Contains(out, "Today's weather") {
		t.Fatalf("results: %s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if len([]rune(line)) > webSnippetChars {
			t.Fatalf("snippet not cut (%d): %.80s", len([]rune(line)), line)
		}
	}
	if (*reqs)[0]["limit"].(float64) != webSearchResults {
		t.Fatalf("limit sent: %v", (*reqs)[0]["limit"])
	}
	if out := search.Run(context.Background(), json.RawMessage(`{"query":"blocked"}`)); out != "search failed: engine_failed: duckduckgo blocked" {
		t.Fatalf("error: %q", out)
	}
	if out := search.Run(context.Background(), json.RawMessage(`{"query":" "}`)); !strings.HasPrefix(out, "search failed") {
		t.Fatalf("empty query: %q", out)
	}
}

func TestReadPage(t *testing.T) {
	page := strings.Repeat("a", webPageChars) + strings.Repeat("b", 5000)
	srv, reqs := fakeDaemon(t, page)
	read := WebTools(srv.URL)[1]
	out := read.Run(context.Background(), json.RawMessage(`{"url":"https://example.com/page"}`))
	if !strings.HasPrefix(out, "Example\nhttps://example.com/page\n\n") || strings.Contains(out, "b") && !strings.Contains(out, "[more:") ||
		!strings.HasSuffix(out, fmt.Sprintf("[more: read_page with offset %d]", webPageChars)) {
		t.Fatalf("first part: %.120s ... %s", out, out[len(out)-60:])
	}
	if (*reqs)[0]["renderMode"] != "request" || (*reqs)[0]["readability"] != true {
		t.Fatalf("fetch request: %v", (*reqs)[0])
	}
	out = read.Run(context.Background(), json.RawMessage(fmt.Sprintf(`{"url":"https://example.com/page","offset":%d}`, webPageChars)))
	if !strings.HasSuffix(out, strings.Repeat("b", 5000)) || strings.Contains(out, "[more:") {
		t.Fatalf("second part: %.200s", out)
	}
	for _, bad := range []string{`{"url":"file:///etc/passwd"}`, `{"url":"example.com"}`} {
		if out := read.Run(context.Background(), json.RawMessage(bad)); !strings.HasPrefix(out, "read failed") {
			t.Fatalf("%s: %q", bad, out)
		}
	}
	if len(*reqs) != 2 {
		t.Fatalf("bad URLs reached the daemon: %d requests", len(*reqs))
	}
}

func TestWebToolsUnreachable(t *testing.T) {
	out := WebTools("http://127.0.0.1:1")[0].Run(context.Background(), json.RawMessage(`{"query":"x"}`))
	if !strings.HasPrefix(out, "search failed: web service unreachable") {
		t.Fatalf("got %q", out)
	}
}
