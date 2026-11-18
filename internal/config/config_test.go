package config

import "testing"

func TestParseWhitelist(t *testing.T) {
	pn, err := ParseWhitelist(" 6281234567890, 6289876543210 ,, ")
	if err != nil || !pn["6281234567890"] || !pn["6289876543210"] || len(pn) != 2 {
		t.Fatalf("phone numbers: %v %v", pn, err)
	}
	// B9: digits with country code only.
	for _, bad := range []string{"+6281234567890", "62 812-3456-7890", "123456789@lid", "6281:3", "abc"} {
		if _, err := ParseWhitelist("6281234567890," + bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func clearProviders(t *testing.T) {
	for _, k := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN", "OPENROUTER_API_KEY", "LLM_BASE_URL", "LLM_API_KEY",
		"LLM_MODEL", "DEFAULT_PROVIDER", "ANTHROPIC_MODEL", "ANTHROPIC_EFFORT", "OPENROUTER_MODEL", "OPENROUTER_EFFORT", "LLM_EFFORT", "WHITELIST"} {
		t.Setenv(k, "")
	}
}

func TestProvider(t *testing.T) {
	clearProviders(t)
	t.Setenv("DEFAULT_PROVIDER", "openrouter")
	if _, err := Load(); err == nil {
		t.Fatal("openrouter without key accepted")
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	c, err := Load()
	if err != nil || c.Provider != "openrouter" || c.Model != "deepseek/deepseek-v4.1-flash-1017" || c.Effort != "low" {
		t.Fatalf("openrouter config: %+v %v", c, err)
	}
	if _, ok := c.Ready["anthropic"]; ok || len(c.Ready) != 1 {
		t.Fatalf("ready without keys: %v", c.Ready)
	}
	t.Setenv("DEFAULT_PROVIDER", "")
	if _, err := Load(); err == nil {
		t.Fatal("anthropic without key accepted")
	}
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("ANTHROPIC_EFFORT", "medium")
	if c, err = Load(); err != nil || c.Provider != "anthropic" || c.Model != "claude-haiku-5-5" || c.Effort != "medium" {
		t.Fatalf("anthropic config: %+v %v", c, err)
	}
	t.Setenv("LLM_BASE_URL", "https://api.example.com/v1")
	t.Setenv("LLM_API_KEY", "k")
	if _, err := Load(); err == nil {
		t.Fatal("openai without LLM_MODEL accepted")
	}
	t.Setenv("LLM_MODEL", "m")
	t.Setenv("LLM_EFFORT", "none")
	t.Setenv("DEFAULT_PROVIDER", "OpenAI")
	if c, err = Load(); err != nil || c.Provider != "openai" || c.Model != "m" || c.Effort != "none" || c.LLMBaseURL != "https://api.example.com/v1" {
		t.Fatalf("openai config: %+v %v", c, err)
	}
	// L13: every provider with credentials is ready to switch to.
	if len(c.Ready) != 3 || c.Ready["anthropic"].Effort != "medium" || c.Ready["openrouter"].Model == "" {
		t.Fatalf("ready: %v", c.Ready)
	}
	t.Setenv("DEFAULT_PROVIDER", "gemini")
	if _, err := Load(); err == nil {
		t.Fatal("unknown provider accepted")
	}
	t.Setenv("DEFAULT_PROVIDER", "")
	t.Setenv("WHITELIST", "+62 812")
	if _, err := Load(); err == nil {
		t.Fatal("bad WHITELIST accepted")
	}
}

func TestEmbedModel(t *testing.T) {
	clearProviders(t)
	t.Setenv("ANTHROPIC_API_KEY", "a")
	t.Setenv("OPENROUTER_API_KEY", "")
	t.Setenv("EMBED_MODEL", "")
	if c, _ := Load(); c.EmbedModel != "" {
		t.Fatalf("embeddings without an OpenRouter key: %q", c.EmbedModel)
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	if c, _ := Load(); c.EmbedModel != "openai/text-embedding-3-small" {
		t.Fatalf("default: %q", c.EmbedModel)
	}
	t.Setenv("EMBED_MODEL", "off")
	if c, _ := Load(); c.EmbedModel != "" {
		t.Fatalf("off: %q", c.EmbedModel)
	}
}
