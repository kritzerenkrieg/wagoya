// Package tools provides the owner-only API tools (I1-I3): a generic
// http_request tool and the tools of configured MCP servers.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
)

const (
	httpTimeout  = 30 * time.Second
	httpMaxBody  = 2 << 20 // bytes read from a response
	httpMaxReply = 20000   // characters of the body given to the model
)

// errBlocked is returned when a request targets a non-public address.
var errBlocked = errors.New("blocked: only public internet addresses are allowed")

// publicOnly refuses connections to loopback, private, link-local (cloud
// metadata), multicast and unspecified addresses. It runs on the resolved IP
// at connect time, so DNS tricks and redirects can't reach the VPS's own
// network (I2).
func publicOnly(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil || !isPublic(ip) {
		return errBlocked
	}
	return nil
}

func isPublic(ip net.IP) bool {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
		// 100.64.0.0/10 carrier-grade NAT, 0.0.0.0/8
		if ip4[0] == 100 && ip4[1]&0xc0 == 64 || ip4[0] == 0 {
			return false
		}
	}
	return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified())
}

// NewHTTPClient returns an http.Client that can only reach public addresses.
func NewHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: publicOnly}
	return &http.Client{
		Timeout: httpTimeout,
		Transport: &http.Transport{
			Proxy:               nil, // a proxy would bypass the address check
			DialContext:         dialer.DialContext,
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConns:        10,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

// HTTPTool is the generic http_request tool (I1): call any public HTTP API.
func HTTPTool(c *http.Client) llm.Tool {
	return llm.Tool{
		Name: "http_request",
		Description: "Make an HTTP request to a public API or URL and get the status, content type and body " +
			"(first 20000 characters). Use it for REST/JSON APIs, including apps whose web pages need JavaScript: " +
			"call their API instead (e.g. exchange a share link for a token, then read the data). " +
			"Don't guess endpoints: first look for the app's API description, e.g. /api/v1/docs.json, /openapi.json, " +
			"/swagger.json, /api/docs, or its documentation site, then follow it. " +
			"Private and local network addresses are blocked.",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"method":  map[string]any{"type": "string", "enum": []string{"GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"}, "description": "Default GET."},
				"url":     map[string]any{"type": "string", "description": "Absolute http(s) URL."},
				"headers": map[string]any{"type": "object", "additionalProperties": map[string]any{"type": "string"}, "description": "Request headers, e.g. Authorization."},
				"body":    map[string]any{"type": "string", "description": "Request body; JSON is sent as application/json unless headers say otherwise."},
				"find":    map[string]any{"type": "string", "description": "Optional: return only lines of the response containing this text (case-insensitive), with 2 lines around each. Use it on big responses like API docs, e.g. find=\"/shares\"."},
				"offset":  map[string]any{"type": "integer", "description": "Optional: start reading the response body at this character, to page through long responses."},
			},
			"required": []string{"url"},
		},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				Method  string            `json:"method"`
				URL     string            `json:"url"`
				Headers map[string]string `json:"headers"`
				Body    string            `json:"body"`
				Find    string            `json:"find"`
				Offset  int               `json:"offset"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "invalid input: " + err.Error()
			}
			return doHTTP(ctx, c, in.Method, in.URL, in.Headers, in.Body, in.Find, in.Offset)
		},
	}
}

func doHTTP(ctx context.Context, c *http.Client, method, rawURL string, headers map[string]string, body, find string, offset int) string {
	if method == "" {
		method = http.MethodGet
	}
	method = strings.ToUpper(method)
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return "invalid url: need an absolute http(s) URL"
	}
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), rd)
	if err != nil {
		return "request error: " + err.Error()
	}
	req.Header.Set("User-Agent", "wagoya (+https://github.com/kritzerenkrieg/wagoya)")
	if body != "" && json.Valid([]byte(body)) {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := c.Do(req)
	if err != nil {
		if errors.Is(err, errBlocked) {
			return errBlocked.Error()
		}
		return "request failed: " + err.Error()
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(res.Body, httpMaxBody))
	text := string(raw)
	head := fmt.Sprintf("status: %s\ncontent-type: %s\nlength: %d characters\n\n", res.Status, res.Header.Get("Content-Type"), len([]rune(text)))
	if find != "" {
		return head + grep(text, find)
	}
	r := []rune(text)
	if offset > 0 {
		if offset >= len(r) {
			return head + "[offset is past the end]"
		}
		r = r[offset:]
	}
	if len(r) > httpMaxReply {
		return head + string(r[:httpMaxReply]) + fmt.Sprintf("\n[truncated: read on with offset=%d, or use find]", offset+httpMaxReply)
	}
	return head + string(r)
}

// grep returns the lines containing find (case-insensitive) with 2 lines of
// context, separated by "--", capped like a normal body.
func grep(text, find string) string {
	lines := strings.Split(text, "\n")
	want := strings.ToLower(find)
	keep := make([]bool, len(lines))
	hits := 0
	for i, l := range lines {
		if strings.Contains(strings.ToLower(l), want) {
			hits++
			for j := max(0, i-2); j <= min(len(lines)-1, i+2); j++ {
				keep[j] = true
			}
		}
	}
	if hits == 0 {
		return fmt.Sprintf("no lines contain %q", find)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d matching lines:\n", hits)
	for i, k := range keep {
		if !k {
			continue
		}
		if i > 0 && !keep[i-1] {
			b.WriteString("--\n")
		}
		b.WriteString(lines[i])
		b.WriteString("\n")
		if b.Len() > httpMaxReply {
			b.WriteString("[truncated: narrow the find text]")
			break
		}
	}
	return b.String()
}
