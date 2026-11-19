// Package llm talks to the model: Claude through the Messages API (API key)
// or the Claude Code CLI in headless mode (subscription token) (A5, A7), or
// any model through OpenRouter (G1).
package llm

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"sync"

	"github.com/anthropics/anthropic-sdk-go/v2"
	"github.com/anthropics/anthropic-sdk-go/v2/option"
)

// Image is an image attached to the request (B8).
type Image struct {
	MediaType string
	Data      []byte
}

// FactSearch looks up stored facts about people in the chat (D7). person is
// a handle like "P1" or ""; query is words to look for or "" to list;
// history also returns facts that are no longer true (J1).
type FactSearch func(ctx context.Context, person, query string, history bool) string

// Tool is a client-side tool the model may call: search_facts, http_request
// or an MCP server's tool (D7, I1). Run gets the model's JSON input and
// returns the text result; errors are reported in that text.
type Tool struct {
	Name        string
	Description string
	Schema      map[string]any // JSON Schema of the input object
	Run         func(ctx context.Context, input json.RawMessage) string
}

// Client sends one stateless request and returns the model's text.
type Client interface {
	// Reply asks the model; tools may be empty. Clients without tool support
	// ignore them, and the caller puts facts in the prompt instead.
	Reply(ctx context.Context, system, prompt string, img *Image, tools []Tool) (string, error)
	SupportsTools() bool
}

const maxToolRounds = 8

// CostReporter is implemented by clients that know what they spent (J6).
type CostReporter interface {
	Cost() float64 // USD
}

// FactSearchTool wraps a FactSearch as the search_facts tool (D7).
func FactSearchTool(search FactSearch) Tool {
	return Tool{
		Name: searchFactsName, Description: searchFactsDesc,
		Schema: map[string]any{"type": "object", "properties": searchFactsProps},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				Person  string `json:"person"`
				Query   string `json:"query"`
				History bool   `json:"history"`
			}
			if json.Unmarshal(input, &in) != nil {
				return "invalid input"
			}
			return search(ctx, in.Person, in.Query, in.History)
		},
	}
}

// findTool returns the tool called name.
func findTool(tools []Tool, name string) (Tool, bool) {
	for _, t := range tools {
		if t.Name == name {
			return t, true
		}
	}
	return Tool{}, false
}

// anthropicTool converts a Tool for the Messages API.
func anthropicTool(t Tool) anthropic.ToolUnionParam {
	schema := anthropic.ToolInputSchemaParam{Properties: t.Schema["properties"], ExtraFields: map[string]any{}}
	for k, v := range t.Schema {
		switch k {
		case "properties", "type":
		case "required":
			if rs, ok := v.([]any); ok {
				for _, r := range rs {
					if s, ok := r.(string); ok {
						schema.Required = append(schema.Required, s)
					}
				}
			} else if rs, ok := v.([]string); ok {
				schema.Required = rs
			}
		default:
			schema.ExtraFields[k] = v
		}
	}
	if schema.Properties == nil {
		schema.Properties = map[string]any{}
	}
	return anthropic.ToolUnionParam{OfTool: &anthropic.ToolParam{Name: t.Name, Description: anthropic.String(t.Description), InputSchema: schema}}
}

// search_facts is shared by the Claude and OpenRouter clients (D7).
const (
	searchFactsName = "search_facts"
	searchFactsDesc = "Search your stored facts about people: anyone you know, from any chat, not only this one. " +
		"Use it before answering anything that depends on who someone is: their name, job, background, preferences, " +
		"history, or what they asked you to remember. Facts are short and start with a label such as Name:, Origin:, Job:, Education:, Skills:, Likes: or Games:, so label words match well. Search with a few short keywords; with no match you get their newest facts instead. " +
		"Results are labelled with the handle, or the name for people not in this chat; people who share a name get an ID tag like Jack #9249, which you can pass as person. " +
		"Notes in brackets tell you how sure a fact is: [unverified] means someone else said it or it was inferred, so don't state it as certain; " +
		"[claim] and [joke] are talk, not facts; [state since ...] is short-lived. Higher-trust facts come first. Outdated facts are hidden unless you set history. Results look like \"fact 123 | P2: Job: nurse\"; the number is the id for invalidate_fact."
)

var searchFactsProps = map[string]any{
	"person":  map[string]any{"type": "string", "description": "Optional: a handle like P1, or a person's name (e.g. Jack) for someone from another chat. Empty searches everyone you know."},
	"query":   map[string]any{"type": "string", "description": "Keywords to look for. Empty, with a person, lists all of that person's facts."},
	"history": map[string]any{"type": "boolean", "description": "Optional: also show facts that are no longer true (old jobs, expired states), e.g. for \"what did he do before?\"."},
}

// ---- Messages API (ANTHROPIC_API_KEY) ----

type apiClient struct {
	c      anthropic.Client
	model  string
	effort string

	costMu sync.Mutex
	cost   float64 // USD from the price card (M4), summed over all requests (J6)
}

// Cost is the total USD this client's requests cost, by the price card.
func (a *apiClient) Cost() float64 {
	a.costMu.Lock()
	defer a.costMu.Unlock()
	return a.cost
}

// NewAPI returns a Messages API client. It sends no server-side web tools:
// web search and page reading are the bot's own tools (W1).
func NewAPI(key, model, effort string) Client {
	return &apiClient{c: anthropic.NewClient(option.WithAPIKey(key)), model: model, effort: claudeEffort(effort)}
}

// claudeEffort maps an effort setting to one Claude takes (low, medium, high,
// xhigh, max). Claude has no "none" or "minimal", so those fall back to its
// lowest, "low", the same setting works on every provider (L13).
func claudeEffort(e string) string {
	switch e {
	case "none", "minimal", "":
		return "low"
	}
	return e
}

const maxRounds = 8 // client tool rounds + pause_turn continuations

// MaxCallTokens caps the input of every model call (W2): above 100k tokens
// some models bill the whole call at a higher rate. The prompt is fitted
// below it, leaving ToolHeadroom for tool rounds; if a round's results would
// still push the next call past it, they are withheld and the model answers
// with what it has.
const (
	MaxCallTokens = 96000
	ToolHeadroom  = 16000
)

// resultBytesPerToken is a deliberately low estimate (more tokens per byte
// than usual), so the cap errs on the safe side.
const resultBytesPerToken = 2.5

const overLimitResult = "[Not shown: this reply has reached its 96k-token limit. Answer now with what you have, and say what you couldn't check.]"

// overLimit reports whether a call of prompt+output tokens plus these new
// tool results would pass MaxCallTokens.
func overLimit(prompt, output int, results []string) bool {
	n := 0
	for _, r := range results {
		n += len(r)
	}
	return prompt+output+int(float64(n)/resultBytesPerToken) > MaxCallTokens
}

func (a *apiClient) SupportsTools() bool { return true }

func (a *apiClient) Reply(ctx context.Context, system, prompt string, img *Image, tools []Tool) (string, error) {
	var blocks []anthropic.ContentBlockParamUnion
	if img != nil {
		blocks = append(blocks, anthropic.NewImageBlockBase64(img.MediaType, base64.StdEncoding.EncodeToString(img.Data)))
	}
	blocks = append(blocks, anthropic.NewTextBlock(prompt))
	params := anthropic.MessageNewParams{
		Model:     anthropic.Model(a.model),
		MaxTokens: 16000,
		// P8: the breakpoint on the system prompt caches tools + system, shared
		// by every request in the next 5 minutes; the top-level one caches the
		// growing conversation, so tool rounds 2+ read what came before.
		System:       []anthropic.TextBlockParam{{Text: system, CacheControl: anthropic.NewCacheControlEphemeralParam()}},
		CacheControl: anthropic.NewCacheControlEphemeralParam(),
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(a.effort)},
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
	}
	for _, t := range tools {
		params.Tools = append(params.Tools, anthropicTool(t))
	}
	// Text is collected across rounds: a long turn can pause (pause_turn)
	// and continue in the next response.
	var out strings.Builder
	clientRounds := 0
	for round := 0; ; round++ {
		if len(params.Tools) > 0 && (round == maxRounds || clientRounds == maxToolRounds) {
			// Last round: make Claude answer with what it has.
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
		resp, err := a.c.Messages.New(ctx, params)
		if err != nil {
			var apiErr *anthropic.Error
			if errors.As(err, &apiErr) {
				return "", fmt.Errorf("claude api %d: %w", apiErr.StatusCode, err)
			}
			return "", err
		}
		if round == 0 { // J5
			reportPromptTokens(ctx, int(resp.Usage.InputTokens+resp.Usage.CacheCreationInputTokens+resp.Usage.CacheReadInputTokens))
		}
		u := resp.Usage
		cost := AnthropicCost(a.model, Usage{Input: u.InputTokens, CacheWrite: u.CacheCreationInputTokens,
			CacheRead: u.CacheReadInputTokens, Output: u.OutputTokens, WebSearches: u.ServerToolUse.WebSearchRequests})
		a.costMu.Lock()
		a.cost += cost
		a.costMu.Unlock()
		if st := statsFrom(ctx); st != nil { // L4, M4
			st.Cost += cost
			st.Rounds++
			st.PromptTokens += int(u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens)
			st.OutputTokens += int(u.OutputTokens)
			st.CachedTokens += int(u.CacheReadInputTokens)
			st.WebSearches += int(u.ServerToolUse.WebSearchRequests)
		}
		// B13: a refusal is sent as is, so just collect whatever text came back.
		for _, b := range resp.Content {
			switch v := b.AsAny().(type) {
			case anthropic.TextBlock:
				out.WriteString(v.Text)
			}
		}
		if resp.StopReason == anthropic.StopReasonPauseTurn && round < maxRounds {
			params.Messages = append(params.Messages, resp.ToParam())
			continue
		}
		if resp.StopReason != anthropic.StopReasonToolUse || len(tools) == 0 {
			return out.String(), nil
		}
		clientRounds++
		params.Messages = append(params.Messages, resp.ToParam())
		var ids, texts []string
		var failed []bool
		for _, b := range resp.Content {
			tu, ok := b.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			ids = append(ids, tu.ID)
			if t, ok := findTool(tools, tu.Name); ok {
				texts, failed = append(texts, t.Run(ctx, json.RawMessage(tu.JSON.Input.Raw()))), append(failed, false)
			} else {
				texts, failed = append(texts, "unknown tool "+tu.Name), append(failed, true)
			}
			log.Printf("tool %s -> %s", tu.Name, firstLine(texts[len(texts)-1], 80)) // as OpenRouter logs them
		}
		prompt := int(resp.Usage.InputTokens + resp.Usage.CacheCreationInputTokens + resp.Usage.CacheReadInputTokens)
		if overLimit(prompt, int(resp.Usage.OutputTokens), texts) { // W2
			log.Printf("tool results withheld: next call would pass %d tokens", MaxCallTokens)
			for i := range texts {
				texts[i] = overLimitResult
			}
			params.ToolChoice = anthropic.ToolChoiceUnionParam{OfNone: &anthropic.ToolChoiceNoneParam{}}
		}
		var results []anthropic.ContentBlockParamUnion
		for i, id := range ids {
			results = append(results, anthropic.NewToolResultBlock(id, texts[i], failed[i]))
		}
		params.Messages = append(params.Messages, anthropic.NewUserMessage(results...))
	}
}

// ---- Claude Code CLI (CLAUDE_CODE_OAUTH_TOKEN) ----

type cliClient struct {
	bin, model, effort, workDir, token string
	web                                bool // E1: Claude Code's WebSearch/WebFetch
}

func NewCLI(bin, token, model, effort, workDir string, web bool) Client {
	return &cliClient{bin: bin, token: token, model: model, effort: claudeEffort(effort), workDir: workDir, web: web}
}

type cliContent struct {
	Type   string     `json:"type"`
	Text   string     `json:"text,omitempty"`
	Source *cliSource `json:"source,omitempty"`
}

type cliSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type"`
	Data      string `json:"data"`
}

// SupportsTools is false: the CLI runs with all tools off, so facts go inline.
func (c *cliClient) SupportsTools() bool { return false }

func (c *cliClient) Reply(ctx context.Context, system, prompt string, img *Image, _ []Tool) (string, error) {
	var content []cliContent
	if img != nil {
		content = append(content, cliContent{Type: "image", Source: &cliSource{
			Type: "base64", MediaType: img.MediaType, Data: base64.StdEncoding.EncodeToString(img.Data)}})
	}
	content = append(content, cliContent{Type: "text", Text: prompt})
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": content},
	})
	if err != nil {
		return "", err
	}

	if err := os.MkdirAll(c.workDir, 0o700); err != nil {
		return "", err
	}
	// Only the web tools, never shell or file tools (B8, E1).
	tools := ""
	if c.web {
		tools = "WebSearch,WebFetch"
	}
	cmd := exec.CommandContext(ctx, c.bin,
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json", "--verbose",
		"--model", c.model,
		"--effort", c.effort,
		"--system-prompt", system,
		"--tools", tools,
		"--allowedTools", tools,
		"--strict-mcp-config",
		"--setting-sources", "",
		"--no-session-persistence",
	)
	cmd.Dir = c.workDir
	cmd.Env = os.Environ()
	if c.token != "" {
		cmd.Env = append(cmd.Env, "CLAUDE_CODE_OAUTH_TOKEN="+c.token)
	}
	cmd.Stdin = strings.NewReader(string(line) + "\n")
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("claude cli: %w: %s", err, strings.TrimSpace(stderr.String()))
	}

	r, ok := ParseCLIResult(out)
	switch {
	case !ok:
		return "", fmt.Errorf("claude cli: no result event: %s", strings.TrimSpace(stderr.String()))
	case r.IsError:
		return "", fmt.Errorf("claude cli error: %s", r.Result)
	}
	if st := statsFrom(ctx); st != nil { // L4, M4: the CLI reports its own cost
		st.Cost += r.CostUSD
		st.Rounds++
	}
	return r.Result, nil
}

// CLIResult is the {"type":"result"} event that ends a Claude Code
// stream-json run.
type CLIResult struct {
	SessionID         string             `json:"session_id"`
	IsError           bool               `json:"is_error"`
	Subtype           string             `json:"subtype"`
	Result            string             `json:"result"`
	CostUSD           float64            `json:"total_cost_usd"`
	PermissionDenials []PermissionDenial `json:"permission_denials"`
}

// PermissionDenial is a tool call Claude Code refused to run.
type PermissionDenial struct {
	ToolName  string          `json:"tool_name"`
	ToolUseID string          `json:"tool_use_id"`
	ToolInput json.RawMessage `json:"tool_input"`
}

// ParseCLIResult finds the last result event in stream-json output.
func ParseCLIResult(out []byte) (CLIResult, bool) {
	var r CLIResult
	found := false
	for _, l := range strings.Split(string(out), "\n") {
		var ev struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(l), &ev) != nil || ev.Type != "result" {
			continue
		}
		var cur CLIResult
		if json.Unmarshal([]byte(l), &cur) == nil {
			r, found = cur, true
		}
	}
	return r, found
}
