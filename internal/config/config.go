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

type Config struct {
	Provider        string // "anthropic" or "openrouter" (G1)
	OpenRouterKey   string
	AnthropicAPIKey string
	OAuthToken      string
	ClaudeBin       string
	WhitelistPN     map[string]bool // phone numbers, digits only (B9)
	WhitelistLID    map[string]bool // LID user parts, for "...@lid" entries (B9)
	Trigger         string
	PairNumber      string
	BotName         string
	Model           string
	Effort          string
	HistoryLimit    int
	MemoryResults   int
	ContextTokens   int
	SendInterval    time.Duration
	WebTools        bool
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

// ParseWhitelist splits WHITELIST into phone numbers and LIDs. Phone numbers
// may contain "+", spaces, dashes or parentheses ("+62 812-3456-7890");
// entries ending in "@lid" are LIDs. Device suffixes (":4") are dropped.
func ParseWhitelist(s string) (pn, lid map[string]bool) {
	pn, lid = map[string]bool{}, map[string]bool{}
	for _, item := range strings.Split(s, ",") {
		item = strings.TrimSpace(item)
		user, server, _ := strings.Cut(item, "@")
		user, _, _ = strings.Cut(user, ":")
		digits := strings.Map(func(r rune) rune {
			if r >= '0' && r <= '9' {
				return r
			}
			return -1
		}, user)
		switch {
		case digits == "":
		case server == "lid":
			lid[digits] = true
		default:
			pn[digits] = true
		}
	}
	return pn, lid
}

func Load() (*Config, error) {
	c := &Config{
		AnthropicAPIKey: get("ANTHROPIC_API_KEY", ""),
		OAuthToken:      get("CLAUDE_CODE_OAUTH_TOKEN", ""),
		ClaudeBin:       get("CLAUDE_BIN", "claude"),
		Trigger:         get("TRIGGER", "!$"),
		PairNumber:      get("PAIR_NUMBER", ""),
		BotName:         get("BOT_NAME", "Claude"),
		Provider:        strings.ToLower(get("PROVIDER", "anthropic")),
		OpenRouterKey:   get("OPENROUTER_API_KEY", ""),
		Effort:          get("EFFORT", "low"),
		DataDir:         get("DATA_DIR", "data"),
		MCPConfig:       get("MCP_CONFIG", ""),
	}
	c.WhitelistPN, c.WhitelistLID = ParseWhitelist(get("WHITELIST", ""))
	var err error
	if c.HistoryLimit, err = getInt("HISTORY_LIMIT", 32); err != nil {
		return nil, err
	}
	if c.MemoryResults, err = getInt("MEMORY_RESULTS", 8); err != nil {
		return nil, err
	}
	if c.ContextTokens, err = getInt("CONTEXT_TOKENS", 128000); err != nil {
		return nil, err
	}
	switch v := strings.ToLower(get("WEB_TOOLS", "true")); v {
	case "true", "1", "yes", "on":
		c.WebTools = true
	case "false", "0", "no", "off":
	default:
		return nil, fmt.Errorf("WEB_TOOLS must be true or false, got %q", v)
	}
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
	switch c.Provider {
	case "anthropic":
		c.Model = get("MODEL", "claude-haiku-5-5")
		if c.AnthropicAPIKey == "" && c.OAuthToken == "" {
			return nil, fmt.Errorf("set ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN")
		}
	case "openrouter":
		c.Model = get("MODEL", "deepseek/deepseek-v4.1-flash-1017")
		if c.OpenRouterKey == "" {
			return nil, fmt.Errorf("PROVIDER=openrouter needs OPENROUTER_API_KEY")
		}
	default:
		return nil, fmt.Errorf("PROVIDER must be anthropic or openrouter, got %q", c.Provider)
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
