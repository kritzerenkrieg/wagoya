package llm

// OpenRouter client (G1-G4): OpenAI-style chat completions over plain
// net/http, no extra dependency. search_facts runs as a function tool in a
// client loop; web search and link reading are OpenRouter server tools that
// run inside the request.

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"regexp"
	"strings"
	"time"
)

const openRouterURL = "https://openrouter.ai/api/v1/chat/completions"

// Caps on OpenRouter's web tools (G3). Searches have no per-request use cap
// outside Anthropic models, so max_total_results bounds them instead.
const (
	orSearchResults      = 5
	orSearchTotalResults = 15
)

// imageFallbackNote is added when the model rejects the image (G4).
const imageFallbackNote = "\n\n[An image was attached, but your current model cannot see images. Say briefly that you couldn't see it.]"

type orClient struct {
	key, model, effort, url string
	web                     bool
	http                    *http.Client
}

// NewOpenRouter returns a client for OpenRouter (G1). model is an OpenRouter
// slug such as "deepseek/deepseek-v4.1-flash-1017".
func NewOpenRouter(key, model, effort string, web bool) Client {
	return &orClient{key: key, model: model, effort: effort, url: openRouterURL, web: web,
		http: &http.Client{Timeout: 3 * time.Minute}}
}

func (c *orClient) SupportsTools() bool { return true }

type orMessage struct {
	Role       string       `json:"role"`
	Content    any          `json:"content"` // string or []orPart
	ToolCalls  []orToolCall `json:"tool_calls,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
}

type orPart struct {
	Type     string      `json:"type"`
	Text     string      `json:"text,omitempty"`
	ImageURL *orImageURL `json:"image_url,omitempty"`
}

type orImageURL struct {
	URL string `json:"url"`
}

type orToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type orRequest struct {
	Model      string         `json:"model"`
	Messages   []orMessage    `json:"messages"`
	Tools      []any          `json:"tools,omitempty"`
	ToolChoice string         `json:"tool_choice,omitempty"`
	MaxTokens  int            `json:"max_tokens"`
	Reasoning  map[string]any `json:"reasoning,omitempty"`
}

type orResponse struct {
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   string       `json:"content"`
			ToolCalls []orToolCall `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// orHTTPError is a non-2xx answer from OpenRouter.
type orHTTPError struct {
	Status int
	Body   string
}

func (e *orHTTPError) Error() string { return fmt.Sprintf("openrouter %d: %s", e.Status, e.Body) }

// orEffort maps EFFORT to OpenRouter's reasoning effort values.
func orEffort(e string) string {
	switch e {
	case "minimal", "low", "medium", "high":
		return e
	case "xhigh", "max":
		return "high"
	}
	return "low"
}

func (c *orClient) tools(tools []Tool) []any {
	var t []any
	for _, tl := range tools {
		schema := tl.Schema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		t = append(t, map[string]any{"type": "function", "function": map[string]any{
			"name": tl.Name, "description": tl.Description, "parameters": schema,
		}})
	}
	if c.web {
		t = append(t,
			map[string]any{"type": "openrouter:web_search", "parameters": map[string]any{
				"max_results": orSearchResults, "max_total_results": orSearchTotalResults}},
			map[string]any{"type": "openrouter:web_fetch", "parameters": map[string]any{
				"max_uses": webFetchMaxUses, "max_content_tokens": webFetchMaxTokens}},
		)
	}
	return t
}

func (c *orClient) Reply(ctx context.Context, system, prompt string, img *Image, tools []Tool) (string, error) {
	out, err := c.reply(ctx, system, prompt, img, tools)
	var he *orHTTPError
	if img != nil && errors.As(err, &he) && (he.Status == http.StatusBadRequest || he.Status == http.StatusNotFound) {
		// G4: the model (or every provider for it) may not take images.
		log.Printf("openrouter rejected the image (%v); retrying without it", err)
		return c.reply(ctx, system, prompt+imageFallbackNote, nil, tools)
	}
	return out, err
}

func (c *orClient) reply(ctx context.Context, system, prompt string, img *Image, tools []Tool) (string, error) {
	var user any = prompt
	if img != nil {
		user = []orPart{
			{Type: "image_url", ImageURL: &orImageURL{URL: "data:" + img.MediaType + ";base64," + base64.StdEncoding.EncodeToString(img.Data)}},
			{Type: "text", Text: prompt},
		}
	}
	req := orRequest{
		Model:     c.model,
		Messages:  []orMessage{{Role: "system", Content: system}, {Role: "user", Content: user}},
		Tools:     c.tools(tools),
		MaxTokens: 16000,
		Reasoning: map[string]any{"effort": orEffort(c.effort), "exclude": true},
	}
	for round := 0; ; round++ {
		if len(tools) > 0 && round == maxToolRounds {
			req.ToolChoice = "none" // last round: answer with what it has
		}
		resp, err := c.post(ctx, req)
		if err != nil {
			return "", err
		}
		if len(resp.Choices) == 0 {
			return "", errors.New("openrouter: no choices in response")
		}
		msg := resp.Choices[0].Message
		var calls []orToolCall
		for _, tc := range msg.ToolCalls {
			if !strings.HasPrefix(tc.Function.Name, "openrouter:") {
				calls = append(calls, tc)
			}
		}
		if len(calls) == 0 || len(tools) == 0 || round == maxToolRounds {
			if strings.TrimSpace(msg.Content) == "" && round > 0 {
				// Tool rounds ran out without an answer: one last call with
				// no tools, so the chat never gets silence.
				req.Tools, req.ToolChoice = nil, ""
				req.Messages = append(req.Messages, orMessage{Role: "user",
					Content: "[No more tool calls. Answer the request now with what you found, and say what you couldn't get.]"})
				if resp, err := c.post(ctx, req); err == nil && len(resp.Choices) > 0 {
					msg.Content = resp.Choices[0].Message.Content
				}
			}
			return stripToolMarkup(msg.Content), nil
		}
		req.Messages = append(req.Messages, orMessage{Role: "assistant", Content: msg.Content, ToolCalls: calls})
		for _, tc := range calls {
			result := "unknown tool " + tc.Function.Name
			if t, ok := findTool(tools, tc.Function.Name); ok {
				args := json.RawMessage(tc.Function.Arguments)
				if len(args) == 0 {
					args = json.RawMessage("{}")
				}
				result = t.Run(ctx, args)
			}
			req.Messages = append(req.Messages, orMessage{Role: "tool", ToolCallID: tc.ID, Content: result})
		}
	}
}

func (c *orClient) post(ctx context.Context, body orRequest) (*orResponse, error) {
	b, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Authorization", "Bearer "+c.key)
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("X-Title", "wagoya")
	res, err := c.http.Do(hr)
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return nil, fmt.Errorf("openrouter: %w", err)
	}
	if res.StatusCode/100 != 2 {
		s := strings.TrimSpace(string(raw))
		if len(s) > 500 {
			s = s[:500]
		}
		return nil, &orHTTPError{Status: res.StatusCode, Body: s}
	}
	var out orResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("openrouter: bad response: %w", err)
	}
	if out.Error != nil {
		return nil, fmt.Errorf("openrouter: %s", out.Error.Message)
	}
	return &out, nil
}

// toolMarkup matches tool-call syntax some models (DeepSeek's "<｜DSML｜...")
// print as plain text when they can't call tools; it must never reach the chat.
var toolMarkup = regexp.MustCompile(`(?s)<[｜|]\s*DSML\s*[｜|].*`)

func stripToolMarkup(s string) string {
	if !toolMarkup.MatchString(s) {
		return s
	}
	out := strings.TrimSpace(toolMarkup.ReplaceAllString(s, ""))
	if out == "" {
		out = "I couldn't finish that: I ran out of tool calls before getting an answer."
	}
	return out
}
