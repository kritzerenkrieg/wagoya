// Package config reads settings from the environment, optionally loading a
// .env file first. Configuration only via .env (AGENTS.md).
package config

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Providers is every model provider, in the order !provider lists them (L13).
var Providers = []string{"anthropic", "openrouter", "openai"}

// ProviderConf is one provider's model and effort (L13).
type ProviderConf struct {
	Model, Effort string
}

type Config struct {
	Provider string // DEFAULT_PROVIDER: "anthropic", "openrouter" (G1) or "openai" (G5)
	// Ready holds the providers whose credentials are set, so !provider can
	// switch to them (L13); Provider is always among them.
	Ready           map[string]ProviderConf
	OpenRouterKey   string
	LLMBaseURL      string // G5: OpenAI-compatible API base, e.g. https://api.abliteration.ai/v1
	LLMKey          string // G5
	AnthropicAPIKey string
	OAuthToken      string
	ClaudeBin       string
	WhitelistPN     map[string]bool // phone numbers, digits with country code (B9)
	Trigger         string
	PairNumber      string
	BotName         string
	Model           string // the default provider's (L13)
	Effort          string
	HistoryLimit    int
	MemoryResults   int
	ContextTokens   int
	SendInterval    time.Duration
	WebTools        bool
	WebSearchURL    string // W1: local open-websearch daemon; "" turns web off
	APITools        bool   // I1: http_request + MCP tools for whitelisted requesters
	MCPConfig       string // I3: path to mcp.json
	EmbedModel      string // J2 c: OpenRouter embedding model for hybrid search; "" means off
	DenyCooldown    time.Duration
	DataDir         string
}

// LoadDotEnv sets variables from path that aren't already set. A missing file
// is not an error (systemd may pass them via EnvironmentFile).
func LoadDotEnv(path string) error {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func get(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func getInt(k string, def int) (int, error) {
	v := get(k, "")
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%s must be a positive integer", k)
	}
	return n, nil
}

func getDuration(k string, def time.Duration) (time.Duration, error) {
	v := get(k, "")
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil || d < 0 {
		return 0, fmt.Errorf("%s must be a duration like 3s or 1m", k)
	}
	return d, nil
}

// ParseWhitelist reads WHITELIST: phone numbers as digits with country
// code, comma-separated, like PAIR_NUMBER ("6281234567890,628987654321").
// Anything else is an error (B9); empty items are skipped.
func ParseWhitelist(s string) (map[string]bool, error) {
	pn := map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if strings.Trim(item, "0123456789") != "" {
			return nil, fmt.Errorf("WHITELIST entry %q: use digits with country code only, like 6281234567890", item)
		}
		pn[item] = true
	}
	return pn, nil
}

func Load() (*Config, error) {
	c := &Config{
		AnthropicAPIKey: get("ANTHROPIC_API_KEY", ""),
		OAuthToken:      get("CLAUDE_CODE_OAUTH_TOKEN", ""),
		ClaudeBin:       get("CLAUDE_BIN", "claude"),
		Trigger:         get("TRIGGER", "!$"),
		PairNumber:      get("PAIR_NUMBER", ""),
		BotName:         get("BOT_NAME", "Claude"),
		Provider:        strings.ToLower(get("DEFAULT_PROVIDER", "anthropic")),
		OpenRouterKey:   get("OPENROUTER_API_KEY", ""),
		LLMBaseURL:      get("LLM_BASE_URL", ""),
		LLMKey:          get("LLM_API_KEY", ""),
		DataDir:         get("DATA_DIR", "data"),
		MCPConfig:       get("MCP_CONFIG", ""),
	}
	var err error
	if c.WhitelistPN, err = ParseWhitelist(get("WHITELIST", "")); err != nil {
		return nil, err
	}
	if c.HistoryLimit, err = getInt("HISTORY_LIMIT", 64); err != nil {
		return nil, err
	}
	if c.MemoryResults, err = getInt("MEMORY_RESULTS", 8); err != nil {
		return nil, err
	}
	if c.ContextTokens, err = getInt("CONTEXT_TOKENS", 96000); err != nil {
		return nil, err
	}
	switch v := strings.ToLower(get("WEB_TOOLS", "true")); v {
	case "true", "1", "yes", "on":
		c.WebTools = true
	case "false", "0", "no", "off":
	default:
		return nil, fmt.Errorf("WEB_TOOLS must be true or false, got %q", v)
	}
	c.WebSearchURL = get("WEBSEARCH_URL", "http://127.0.0.1:3210")
	switch v := strings.ToLower(get("API_TOOLS", "true")); v {
	case "true", "1", "yes", "on":
		c.APITools = true
	case "false", "0", "no", "off":
	default:
		return nil, fmt.Errorf("API_TOOLS must be true or false, got %q", v)
	}
	if c.MCPConfig == "" {
		c.MCPConfig = filepath.Join(c.DataDir, "mcp.json")
	}
	if c.SendInterval, err = getDuration("SEND_INTERVAL", 3*time.Second); err != nil {
		return nil, err
	}
	if c.DenyCooldown, err = getDuration("DENY_COOLDOWN", time.Minute); err != nil {
		return nil, err
	}
	// L13: each provider has its own model and effort; it is ready when its
	// credentials are set.
	c.Ready = map[string]ProviderConf{}
	if c.AnthropicAPIKey != "" || c.OAuthToken != "" {
		c.Ready["anthropic"] = ProviderConf{get("ANTHROPIC_MODEL", "claude-haiku-5-5"), get("ANTHROPIC_EFFORT", "low")}
	}
	if c.OpenRouterKey != "" {
		c.Ready["openrouter"] = ProviderConf{get("OPENROUTER_MODEL", "deepseek/deepseek-v4.1-flash-1017"), get("OPENROUTER_EFFORT", "low")}
	}
	if c.LLMBaseURL != "" || c.LLMKey != "" { // G5: any OpenAI-compatible chat completions API
		m := get("LLM_MODEL", "")
		if c.LLMBaseURL == "" || c.LLMKey == "" || m == "" {
			return nil, fmt.Errorf("the openai provider needs LLM_BASE_URL, LLM_API_KEY and LLM_MODEL")
		}
		c.Ready["openai"] = ProviderConf{m, get("LLM_EFFORT", "low")}
	}
	pc, ok := c.Ready[c.Provider]
	switch {
	case ok:
		c.Model, c.Effort = pc.Model, pc.Effort
	case c.Provider == "anthropic":
		return nil, fmt.Errorf("set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN")
	case c.Provider == "openrouter":
		return nil, fmt.Errorf("DEFAULT_PROVIDER=openrouter needs OPENROUTER_API_KEY")
	case c.Provider == "openai":
		return nil, fmt.Errorf("DEFAULT_PROVIDER=openai needs LLM_BASE_URL, LLM_API_KEY and LLM_MODEL")
	default:
		return nil, fmt.Errorf("DEFAULT_PROVIDER must be %s, got %q", strings.Join(Providers, ", "), c.Provider)
	}
	// J2 c: embeddings go through OpenRouter, so they need its key.
	switch m := get("EMBED_MODEL", "openai/text-embedding-3-small"); strings.ToLower(m) {
	case "off", "none", "false", "0":
	default:
		if c.OpenRouterKey != "" {
			c.EmbedModel = m
		}
	}
	return c, nil
}
