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
		"Results are labelled with the handle, or the name for people not in this chat; people who share a name get an ID tag like Omar #9249, which you can pass as person. " +
		"Notes in brackets tell you how sure a fact is: [unverified] means someone else said it or it was inferred, so don't state it as certain; " +
		"[claim] and [joke] are talk, not facts; [state since ...] is short-lived. Higher-trust facts come first. Outdated facts are hidden unless you set history. Results look like \"fact 123 | P2: Job: nurse\"; the number is the id for invalidate_fact."
)

var searchFactsProps = map[string]any{
	"person":  map[string]any{"type": "string", "description": "Optional: a handle like P1, or a person's name (e.g. Omar) for someone from another chat. Empty searches everyone you know."},
	"query":   map[string]any{"type": "string", "description": "Keywords to look for. Empty, with a person, lists all of that person's facts."},
	"history": map[string]any{"type": "boolean", "description": "Optional: also show facts that are no longer true (old jobs, expired states), e.g. for \"what did he do before?\"."},
}

// ---- Messages API (ANTHROPIC_API_KEY) ----

type apiClient struct {
	c      anthropic.Client
	model  string
	effort string
	web    bool // E1: Anthropic server-side web_search and web_fetch
}

func NewAPI(key, model, effort string, web bool) Client {
	return &apiClient{c: anthropic.NewClient(option.WithAPIKey(key)), model: model, effort: effort, web: web}
}

// Per-request caps on the server-side web tools (E1), to bound cost.
const (
	webSearchMaxUses  = 6
	webFetchMaxUses   = 6
	webFetchMaxTokens = 20000
	maxRounds         = 8 // client tool rounds + pause_turn continuations
)

func (a *apiClient) SupportsTools() bool { return true }

func (a *apiClient) Reply(ctx context.Context, system, prompt string, img *Image, tools []Tool) (string, error) {
	var blocks []anthropic.ContentBlockParamUnion
	if img != nil {
		blocks = append(blocks, anthropic.NewImageBlockBase64(img.MediaType, base64.StdEncoding.EncodeToString(img.Data)))
	}
	blocks = append(blocks, anthropic.NewTextBlock(prompt))
	params := anthropic.MessageNewParams{
		Model:        anthropic.Model(a.model),
		MaxTokens:    16000,
		System:       []anthropic.TextBlockParam{{Text: system}},
		OutputConfig: anthropic.OutputConfigParam{Effort: anthropic.OutputConfigEffort(a.effort)},
		Messages:     []anthropic.MessageParam{anthropic.NewUserMessage(blocks...)},
	}
	for _, t := range tools {
		params.Tools = append(params.Tools, anthropicTool(t))
	}
	if a.web {
		params.Tools = append(params.Tools, webTools(a.model)...)
	}
	// Text is collected across rounds: a long server-side web turn can pause
	// (pause_turn) and continue in the next response.
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
		// B13: a refusal is sent as is, so just collect whatever text came back.
		for _, b := range resp.Content {
			switch v := b.AsAny().(type) {
			case anthropic.TextBlock:
				out.WriteString(v.Text)
			case anthropic.ServerToolUseBlock:
				if os.Getenv("LLM_DEBUG") != "" {
					log.Printf("server tool %s input %s", v.Name, v.JSON.Input.Raw())
				}
			case anthropic.WebSearchToolResultBlock: // E1: log server tool failures
				if v.Content.ErrorCode != "" {
					log.Printf("web_search error: %s", v.Content.ErrorCode)
				}
			case anthropic.WebFetchToolResultBlock:
				if v.Content.ErrorCode != "" {
					log.Printf("web_fetch error: %s", v.Content.ErrorCode)
				}
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
		var results []anthropic.ContentBlockParamUnion
		for _, b := range resp.Content {
			tu, ok := b.AsAny().(anthropic.ToolUseBlock)
			if !ok {
				continue
			}
			t, ok := findTool(tools, tu.Name)
			if !ok {
				results = append(results, anthropic.NewToolResultBlock(tu.ID, "unknown tool "+tu.Name, true))
				continue
			}
			results = append(results, anthropic.NewToolResultBlock(tu.ID, t.Run(ctx, json.RawMessage(tu.JSON.Input.Raw())), false))
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
	return &cliClient{bin: bin, token: token, model: model, effort: effort, workDir: workDir, web: web}
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

// webTools picks the server web tool versions (E1). The dynamic-filtering
// 20260209 versions are documented for Opus and Sonnet; Haiku sends malformed
// input to them ({"params": ...}), which fails and still uses up max_uses, so
// Haiku gets the basic versions.
func webTools(model string) []anthropic.ToolUnionParam {
	if strings.Contains(model, "haiku") {
		return []anthropic.ToolUnionParam{
			{OfWebSearchTool20250305: &anthropic.WebSearchTool20250305Param{MaxUses: anthropic.Int(webSearchMaxUses)}},
			{OfWebFetchTool20250910: &anthropic.WebFetchTool20250910Param{
				MaxUses: anthropic.Int(webFetchMaxUses), MaxContentTokens: anthropic.Int(webFetchMaxTokens)}},
		}
	}
	return []anthropic.ToolUnionParam{
		{OfWebSearchTool20260209: &anthropic.WebSearchTool20260209Param{MaxUses: anthropic.Int(webSearchMaxUses)}},
		{OfWebFetchTool20260209: &anthropic.WebFetchTool20260209Param{
			MaxUses: anthropic.Int(webFetchMaxUses), MaxContentTokens: anthropic.Int(webFetchMaxTokens)}},
	}
}
