package main

// R1, R4: Claude Code runs with no allow or deny list of the runner's own,
// in auto mode (R6). Whenever it needs a permission it calls the MCP tool
// below (--permission-prompt-tool), which the runner binary serves itself
// (`claude-runner approve-mcp <socket>`). The tool hands the request to
// the runner over a unix socket; the runner posts it to the owner through
// the bot and waits for their yes or no.

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	approveTool  = "mcp__wagoya__approve"
	approvalWait = 24 * time.Hour // then the request is denied (R4)
	// mcpToolTimeout keeps Claude Code waiting on the tool past approvalWait.
	mcpToolTimeout = "90000000" // ms, 25 h
)

// permRequest is what Claude Code passes the permission tool.
type permRequest struct {
	ToolName  string          `json:"tool_name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
}

type permAnswer struct {
	Allow bool `json:"allow"`
}

// serveMCP is the permission tool: a minimal MCP server on stdin/stdout.
func serveMCP(socket string, in io.Reader, out io.Writer) error {
	r := bufio.NewReaderSize(in, 1<<20)
	enc := json.NewEncoder(out)
	for {
		line, err := r.ReadBytes('\n')
		if len(strings.TrimSpace(string(line))) > 0 {
			var m struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if json.Unmarshal(line, &m) == nil && len(m.ID) > 0 && string(m.ID) != "null" {
				res, rpcErr := mcpCall(socket, m.Method, m.Params)
				msg := map[string]any{"jsonrpc": "2.0", "id": m.ID}
				if rpcErr != "" {
					msg["error"] = map[string]any{"code": -32601, "message": rpcErr}
				} else {
					msg["result"] = res
				}
				if err := enc.Encode(msg); err != nil {
					return err
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
	}
}

func mcpCall(socket, method string, params json.RawMessage) (any, string) {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = "2024-11-05"
		}
		return map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "wagoya", "version": "1"}}, ""
	case "ping":
		return map[string]any{}, ""
	case "tools/list":
		return map[string]any{"tools": []any{map[string]any{
			"name":        "approve",
			"description": "Asks the owner, over WhatsApp, to allow a tool call.",
			"inputSchema": map[string]any{"type": "object", "properties": map[string]any{
				"tool_name": map[string]any{"type": "string"}, "input": map[string]any{"type": "object"},
				"tool_use_id": map[string]any{"type": "string"}}},
		}}}, ""
	case "tools/call":
		var p struct {
			Arguments permRequest `json:"arguments"`
		}
		json.Unmarshal(params, &p)
		allow := askRunner(socket, p.Arguments)
		var v any = map[string]any{"behavior": "deny", "message": "The owner did not allow this (asked over WhatsApp). Don't work around it; say in your final reply what you wanted to do and why."}
		if allow {
			input := p.Arguments.Input
			if len(input) == 0 {
				input = json.RawMessage(`{}`)
			}
			v = map[string]any{"behavior": "allow", "updatedInput": input}
		}
		b, _ := json.Marshal(v)
		return map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}}, ""
	}
	return nil, "method not found: " + method
}

// askRunner passes one request to the runner and waits; any failure denies.
func askRunner(socket string, req permRequest) bool {
	c, err := net.Dial("unix", socket)
	if err != nil {
		return false
	}
	defer c.Close()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return false
	}
	var a permAnswer
	if err := json.NewDecoder(c).Decode(&a); err != nil {
		return false
	}
	return a.Allow
}

// approver is the runner's side for one run: it relays each request to the
// owner, one at a time, and remembers what the owner denied.
type approver struct {
	q       queue
	id      int64
	waiting atomic.Int32 // requests waiting on the owner; the run clock stops (R6)
	mu      sync.Mutex   // one request at a time: the task has one slot
	dmu     sync.Mutex
	denied  map[string]bool // tool_use_ids the owner said no to
	poll    func(waited time.Duration) time.Duration
}

func newApprover(q queue, id int64) *approver {
	return &approver{q: q, id: id, denied: map[string]bool{}, poll: func(waited time.Duration) time.Duration {
		if waited < 10*time.Minute {
			return 5 * time.Second
		}
		return 30 * time.Second
	}}
}

// serve answers requests on ln until it is closed.
func (a *approver) serve(ctx context.Context, ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer c.Close()
			var req permRequest
			if err := json.NewDecoder(c).Decode(&req); err != nil {
				return
			}
			json.NewEncoder(c).Encode(permAnswer{Allow: a.ask(ctx, req)})
		}()
	}
}

func (a *approver) ask(ctx context.Context, req permRequest) bool {
	a.waiting.Add(1)
	defer a.waiting.Add(-1)
	a.mu.Lock()
	defer a.mu.Unlock()
	text := describe(req.ToolName, req.Input)
	actx, cancel := context.WithTimeout(ctx, time.Minute)
	err := a.q.Ask(actx, a.id, text)
	cancel()
	if err != nil {
		log.Printf("task #%d: ask the owner: %v", a.id, err)
		a.deny(req.ToolUseID)
		return false
	}
	log.Printf("task #%d: asked the owner: %s", a.id, firstLine(text))
	start := time.Now()
	for time.Since(start) < approvalWait {
		select {
		case <-ctx.Done():
			return false
		case <-time.After(a.poll(time.Since(start))):
		}
		actx, cancel := context.WithTimeout(ctx, time.Minute)
		ans, err := a.q.Answer(actx, a.id)
		cancel()
		switch {
		case err != nil:
			log.Printf("task #%d: approval poll: %v", a.id, err) // a flaky link: keep waiting
		case ans == "allow":
			log.Printf("task #%d: the owner allowed it", a.id)
			return true
		case ans == "deny":
			log.Printf("task #%d: the owner denied it", a.id)
			a.deny(req.ToolUseID)
			return false
		}
	}
	a.deny(req.ToolUseID)
	return false
}

func (a *approver) deny(id string) {
	a.dmu.Lock()
	a.denied[id] = true
	a.dmu.Unlock()
}

func (a *approver) wasDenied(id string) bool {
	a.dmu.Lock()
	defer a.dmu.Unlock()
	return id != "" && a.denied[id]
}

// describe says what a tool call does, for the owner (WhatsApp formatting).
func describe(tool string, input json.RawMessage) string {
	var in struct {
		Command     string `json:"command"`
		Description string `json:"description"`
		FilePath    string `json:"file_path"`
		NotebookP   string `json:"notebook_path"`
		URL         string `json:"url"`
		Query       string `json:"query"`
		Path        string `json:"path"`
	}
	json.Unmarshal(input, &in)
	code := func(s string) string {
		s = clipRunes(strings.TrimSpace(s), 1500)
		if strings.Contains(s, "\n") || strings.Contains(s, "`") {
			return "```\n" + s + "\n```"
		}
		return "`" + s + "`"
	}
	switch {
	case tool == "Bash" && in.Command != "":
		s := "Run " + code(in.Command)
		if d := strings.TrimSpace(in.Description); d != "" {
			s += "\n_" + clipRunes(d, 200) + "_"
		}
		return s
	case (tool == "Write" || tool == "Edit" || tool == "MultiEdit") && in.FilePath != "":
		verb := map[string]string{"Write": "Write", "Edit": "Edit", "MultiEdit": "Edit"}[tool]
		return verb + " " + code(in.FilePath)
	case tool == "NotebookEdit" && in.NotebookP != "":
		return "Edit " + code(in.NotebookP)
	case tool == "WebFetch" && in.URL != "":
		return "Open " + in.URL
	case tool == "WebSearch" && in.Query != "":
		return "Search the web for " + code(in.Query)
	}
	raw := strings.TrimSpace(string(input))
	if raw == "" || raw == "{}" {
		return "Use " + tool
	}
	return fmt.Sprintf("Use %s with %s", tool, code(clipRunes(raw, 500)))
}

// blocked lists the actions Claude Code was refused that the owner wasn't
// already asked about, and the rules that allow them on resume (R4).
func blocked(ds []permDenial, asked func(string) bool) (text string, rules []string) {
	seen := map[string]bool{}
	var b strings.Builder
	for _, d := range ds {
		if asked(d.ToolUseID) {
			continue
		}
		rule := d.ToolName
		var in struct {
			Command string `json:"command"`
		}
		if json.Unmarshal(d.ToolInput, &in) == nil && d.ToolName == "Bash" && strings.TrimSpace(in.Command) != "" {
			rule = "Bash(" + strings.TrimSpace(in.Command) + ")"
		}
		if seen[rule] {
			continue
		}
		seen[rule] = true
		rules = append(rules, rule)
		line := describe(d.ToolName, d.ToolInput)
		if d.ToolName != "Bash" {
			line += " (allows " + d.ToolName + " for the rest of the task)"
		}
		b.WriteString("- " + line + "\n")
	}
	return strings.TrimSpace(b.String()), rules
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	return s
}

func clipRunes(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}
