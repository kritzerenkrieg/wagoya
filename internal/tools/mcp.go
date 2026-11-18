package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
)

// MCPConfig is the mcp.json format shared with Claude Desktop and Claude
// Code (I3): {"mcpServers": {"name": {"command", "args", "env"} | {"url", "headers"}}}.
type MCPConfig struct {
	Servers map[string]MCPServer `json:"mcpServers"`
}

// MCPServer is one server: a local command (stdio) or a remote URL
// (streamable HTTP).
type MCPServer struct {
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// ReadMCPConfig reads path. A missing file means no servers.
func ReadMCPConfig(path string) (MCPConfig, error) {
	var c MCPConfig
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// MCP holds the connected servers.
type MCP struct {
	client *mcp.Client
	mu     sync.Mutex
	conns  map[string]*mcpConn
	tools  []llm.Tool
}

type mcpConn struct {
	name    string
	cfg     MCPServer
	session *mcp.ClientSession
}

const (
	mcpCallTimeout = 60 * time.Second
	mcpMaxReply    = 20000
)

var toolNameUnsafe = regexp.MustCompile(`[^a-zA-Z0-9_-]+`)

// toolName makes "mcp__server__tool", trimmed to the 64-character limit of
// model tool names.
func toolName(server, tool string) string {
	n := "mcp__" + toolNameUnsafe.ReplaceAllString(server, "_") + "__" + toolNameUnsafe.ReplaceAllString(tool, "_")
	if len(n) > 64 {
		n = n[:64]
	}
	return n
}

// ConnectMCP connects every configured server and lists its tools. A server
// that fails is logged and skipped; the bot runs without it.
func ConnectMCP(ctx context.Context, cfg MCPConfig) *MCP {
	m := &MCP{
		client: mcp.NewClient(&mcp.Implementation{Name: "wagoya", Version: "1"}, nil),
		conns:  map[string]*mcpConn{},
	}
	names := make([]string, 0, len(cfg.Servers))
	for n := range cfg.Servers {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, name := range names {
		c := &mcpConn{name: name, cfg: cfg.Servers[name]}
		if err := m.connect(ctx, c); err != nil {
			log.Printf("mcp %s: %v", name, err)
			continue
		}
		n := 0
		for t, err := range c.session.Tools(ctx, nil) {
			if err != nil {
				log.Printf("mcp %s: list tools: %v", name, err)
				break
			}
			m.tools = append(m.tools, m.wrap(c, t))
			n++
		}
		m.conns[name] = c
		log.Printf("mcp %s: connected, %d tools", name, n)
	}
	return m
}

func (m *MCP) connect(ctx context.Context, c *mcpConn) error {
	var t mcp.Transport
	switch {
	case c.cfg.Command != "":
		cmd := exec.Command(c.cfg.Command, c.cfg.Args...)
		cmd.Env = os.Environ()
		for k, v := range c.cfg.Env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
		t = &mcp.CommandTransport{Command: cmd}
	case c.cfg.URL != "":
		t = &mcp.StreamableClientTransport{Endpoint: c.cfg.URL,
			HTTPClient: &http.Client{Transport: headerTransport{c.cfg.Headers, http.DefaultTransport}}}
	default:
		return errors.New("needs a command or a url")
	}
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s, err := m.client.Connect(cctx, t, nil)
	if err != nil {
		return err
	}
	c.session = s
	return nil
}

// headerTransport adds configured headers (e.g. Authorization) to remote MCP
// requests.
type headerTransport struct {
	headers map[string]string
	next    http.RoundTripper
}

func (h headerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	for k, v := range h.headers {
		r.Header.Set(k, v)
	}
	return h.next.RoundTrip(r)
}

func (m *MCP) wrap(c *mcpConn, t *mcp.Tool) llm.Tool {
	schema := map[string]any{"type": "object", "properties": map[string]any{}}
	if b, err := json.Marshal(t.InputSchema); err == nil {
		var s map[string]any
		if json.Unmarshal(b, &s) == nil && s != nil {
			schema = s
		}
	}
	desc := t.Description
	if desc == "" {
		desc = t.Name
	}
	remote := t.Name
	return llm.Tool{
		Name:        toolName(c.name, remote),
		Description: fmt.Sprintf("[MCP server %q] %s", c.name, desc),
		Schema:      schema,
		Run: func(ctx context.Context, input json.RawMessage) string {
			return m.call(ctx, c, remote, input)
		},
	}
}

// call runs a tool, reconnecting once if the session died.
func (m *MCP) call(ctx context.Context, c *mcpConn, tool string, input json.RawMessage) string {
	var args map[string]any
	if len(input) > 0 && json.Unmarshal(input, &args) != nil {
		return "invalid input: arguments must be a JSON object"
	}
	ctx, cancel := context.WithTimeout(ctx, mcpCallTimeout)
	defer cancel()
	m.mu.Lock()
	s := c.session
	m.mu.Unlock()
	res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil && ctx.Err() == nil {
		log.Printf("mcp %s: %s failed (%v), reconnecting", c.name, tool, err)
		m.mu.Lock()
		_ = c.session.Close()
		rerr := m.connect(ctx, c)
		s = c.session
		m.mu.Unlock()
		if rerr == nil {
			res, err = s.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: args})
		}
	}
	if err != nil {
		return "tool call failed: " + err.Error()
	}
	return renderResult(res)
}

func renderResult(res *mcp.CallToolResult) string {
	var b strings.Builder
	if res.IsError {
		b.WriteString("[tool error] ")
	}
	for _, c := range res.Content {
		switch v := c.(type) {
		case *mcp.TextContent:
			b.WriteString(v.Text)
			b.WriteString("\n")
		default:
			if j, err := json.Marshal(v); err == nil {
				b.Write(j)
				b.WriteString("\n")
			}
		}
	}
	if b.Len() == 0 && res.StructuredContent != nil {
		if j, err := json.Marshal(res.StructuredContent); err == nil {
			b.Write(j)
		}
	}
	out := strings.TrimSpace(b.String())
	if r := []rune(out); len(r) > mcpMaxReply {
		out = string(r[:mcpMaxReply]) + "\n[truncated]"
	}
	if out == "" {
		out = "(no output)"
	}
	return out
}

// Tools returns the wrapped tools of all connected servers.
func (m *MCP) Tools() []llm.Tool {
	if m == nil {
		return nil
	}
	return m.tools
}

// Close ends every session (and stops local server processes).
func (m *MCP) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.conns {
		_ = c.session.Close()
	}
}
