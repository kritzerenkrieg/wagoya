package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestIsPublic(t *testing.T) {
	for ip, want := range map[string]bool{
		"8.8.8.8": true, "1.1.1.1": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "192.168.1.1": false, "172.16.0.1": false,
		"169.254.169.254": false, "100.64.0.1": false, "0.0.0.0": false, "::1": false,
		"fe80::1": false, "fd00::1": false, "::ffff:127.0.0.1": false, "224.0.0.1": false,
	} {
		if got := isPublic(net.ParseIP(ip)); got != want {
			t.Errorf("isPublic(%s) = %v, want %v", ip, got, want)
		}
	}
}

func TestHTTPToolBlocksLocal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("secret")) }))
	defer srv.Close()
	tool := HTTPTool(NewHTTPClient(), nil)
	out := tool.Run(context.Background(), json.RawMessage(`{"url":"`+srv.URL+`"}`))
	if !strings.Contains(out, "blocked") || strings.Contains(out, "secret") {
		t.Fatalf("local server reachable: %q", out)
	}
	for _, in := range []string{`{"url":"file:///etc/passwd"}`, `{"url":"/relative"}`, `{bad`} {
		if out := tool.Run(context.Background(), json.RawMessage(in)); !strings.HasPrefix(out, "invalid") {
			t.Errorf("%s: %q", in, out)
		}
	}
}

func TestHTTPToolRequest(t *testing.T) {
	var got *http.Request
	var body string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r
		b := make([]byte, 100)
		n, _ := r.Body.Read(b)
		body = string(b[:n])
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"token":"t"}` + strings.Repeat("x", httpMaxReply)))
	}))
	defer srv.Close()
	// A plain client: this test checks request building, not address blocking.
	out := doHTTP(context.Background(), srv.Client(), nil, "post", srv.URL+"/api/v1/shares/abc/auth",
		map[string]string{"Authorization": "Bearer k"}, `{"password":""}`, "", 0)
	if got.Method != "POST" || got.URL.Path != "/api/v1/shares/abc/auth" || got.Header.Get("Authorization") != "Bearer k" ||
		got.Header.Get("Content-Type") != "application/json" || body != `{"password":""}` {
		t.Fatalf("request: %s %s %v body=%q", got.Method, got.URL, got.Header, body)
	}
	if !strings.HasPrefix(out, "status: 200 OK\ncontent-type: application/json\nlength: 20013 characters\n\n{\"token\":\"t\"}") ||
		!strings.HasSuffix(out, "[truncated: read on with offset=20000, or use find]") {
		t.Fatalf("output: %.120q ... %q", out, out[len(out)-60:])
	}
	if out := doHTTP(context.Background(), srv.Client(), nil, "GET", srv.URL, nil, "", "", 20000); !strings.HasSuffix(out, "\n\nxxxxxxxxxxxxx") {
		t.Fatalf("offset: %q", out[len(out)-30:])
	}
}

func TestGrep(t *testing.T) {
	doc := "a\nb\n\"/shares/{share}/auth\": {\nc\nd\ne\nf\ng\n\"/projects\": {\nh"
	got := grep(doc, "/SHARES")
	if !strings.HasPrefix(got, "1 matching lines:\na\nb\n\"/shares/{share}/auth\": {\nc\nd\n") || strings.Contains(got, "projects") {
		t.Fatalf("grep: %q", got)
	}
	if grep(doc, "nope") != `no lines contain "nope"` {
		t.Fatal("no-match message")
	}
}

func TestMCPRoundTrip(t *testing.T) {
	ctx := context.Background()
	server := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	type in struct {
		Name string `json:"name"`
	}
	mcp.AddTool(server, &mcp.Tool{Name: "greet.user", Description: "Say hi"},
		func(_ context.Context, _ *mcp.CallToolRequest, a in) (*mcp.CallToolResult, any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "hi " + a.Name}}}, nil, nil
		})
	st, ct := mcp.NewInMemoryTransports()
	if _, err := server.Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	m := &MCP{client: mcp.NewClient(&mcp.Implementation{Name: "wagoya", Version: "1"}, nil), conns: map[string]*mcpConn{}}
	s, err := m.client.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	c := &mcpConn{name: "my server", session: s}
	m.conns[c.name] = c
	var tools []string
	for tl, err := range s.Tools(ctx, nil) {
		if err != nil {
			t.Fatal(err)
		}
		w := m.wrap(c, tl)
		tools = append(tools, w.Name)
		if w.Name != "mcp__my_server__greet_user" || !strings.Contains(w.Description, "Say hi") || w.Schema["type"] != "object" {
			t.Fatalf("wrapped tool: %+v", w)
		}
		if out := w.Run(ctx, json.RawMessage(`{"name":"John"}`)); out != "hi John" {
			t.Fatalf("call: %q", out)
		}
		if out := w.Run(ctx, json.RawMessage(`[1]`)); !strings.HasPrefix(out, "invalid input") {
			t.Fatalf("bad input: %q", out)
		}
	}
	if len(tools) != 1 {
		t.Fatalf("tools: %v", tools)
	}
	m.Close()
}

func TestReadMCPConfig(t *testing.T) {
	if c, err := ReadMCPConfig(t.TempDir() + "/none.json"); err != nil || len(c.Servers) != 0 {
		t.Fatalf("missing file: %+v %v", c, err)
	}
	if len(toolName(strings.Repeat("s", 50), strings.Repeat("t", 50))) != 64 {
		t.Fatal("tool name not capped at 64")
	}
}

// I4: stored keys go only to their host over https, never reach the model,
// and are dropped when a redirect leaves https on that host.
func TestStoredKeys(t *testing.T) {
	const secret = "Bearer tok-0123456789abcdef"
	var leaked atomic.Bool
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			leaked.Store(true)
		}
		fmt.Fprint(w, "plain ok")
	}))
	defer plain.Close()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/away":
			http.Redirect(w, r, plain.URL+"/x", http.StatusFound)
		default:
			if r.Header.Get("Authorization") != secret {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmt.Fprintf(w, "hello, your header was %s", r.Header.Get("Authorization"))
		}
	}))
	defer srv.Close()
	keys := Keys{"127.0.0.1": {"Authorization": secret}}
	run := func(k Keys, url string, headers map[string]string) string {
		return k.scrub(doHTTP(context.Background(), srv.Client(), k, "GET", url, headers, "", "", 0))
	}
	out := run(keys, srv.URL+"/tasks", map[string]string{"Authorization": "Bearer wrong"})
	if !strings.Contains(out, "200 OK") || strings.Contains(out, "tok-0123") || !strings.Contains(out, "[stored credential]") {
		t.Fatalf("owner request: %q", out)
	}
	if out := run(nil, srv.URL+"/tasks", nil); !strings.Contains(out, "401") {
		t.Fatalf("request without keys got: %q", out)
	}
	if out := run(keys, srv.URL+"/away", nil); !strings.Contains(out, "plain ok") || leaked.Load() {
		t.Fatalf("redirect off https kept the key: leaked=%v %q", leaked.Load(), out)
	}
	if out := run(keys, plain.URL+"/x", nil); leaked.Load() {
		t.Fatalf("key sent over plain http: %q", out)
	}
	if d := HTTPTool(NewHTTPClient(), keys).Description; !strings.Contains(d, "127.0.0.1") || strings.Contains(d, "tok-") {
		t.Fatalf("description: %q", d)
	}
}

func TestLoadKeys(t *testing.T) {
	dir := t.TempDir()
	if k, err := LoadKeys(filepath.Join(dir, "none.json")); k != nil || err != nil {
		t.Fatalf("missing file: %v %v", k, err)
	}
	p := filepath.Join(dir, "k.json")
	os.WriteFile(p, []byte(`{"Tasks.Example.com": {"Authorization": "Bearer x"}}`), 0o600)
	if k, err := LoadKeys(p); err != nil || k["tasks.example.com"]["Authorization"] != "Bearer x" {
		t.Fatalf("got %v %v", k, err)
	}
	os.WriteFile(p, []byte(`{"https://tasks.example.com/": {"Authorization": "Bearer x"}}`), 0o600)
	if _, err := LoadKeys(p); err == nil {
		t.Fatal("URL accepted as host")
	}
}
