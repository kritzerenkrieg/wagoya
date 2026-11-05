package config

import "testing"

func TestParseWhitelist(t *testing.T) {
	pn, lid := ParseWhitelist(" +62 812-3456-7890, 6289876543210:3@s.whatsapp.net , 123456789@lid,, abc")
	if !pn["6281234567890"] || !pn["6289876543210"] || len(pn) != 2 {
		t.Fatalf("phone numbers: %v", pn)
	}
	if !lid["123456789"] || len(lid) != 1 {
		t.Fatalf("lids: %v", lid)
	}
}

func TestProvider(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("MODEL", "")
	t.Setenv("PROVIDER", "openrouter")
	t.Setenv("OPENROUTER_API_KEY", "")
	if _, err := Load(); err == nil {
		t.Fatal("openrouter without key accepted")
	}
	t.Setenv("OPENROUTER_API_KEY", "k")
	c, err := Load()
	if err != nil || c.Provider != "openrouter" || c.Model != "deepseek/deepseek-v4.1-flash-1017" {
		t.Fatalf("openrouter config: %+v %v", c, err)
	}
	t.Setenv("PROVIDER", "")
	if _, err := Load(); err == nil {
		t.Fatal("anthropic without key accepted")
	}
	t.Setenv("ANTHROPIC_API_KEY", "a")
	if c, err = Load(); err != nil || c.Provider != "anthropic" || c.Model != "claude-haiku-5-5" {
		t.Fatalf("anthropic config: %+v %v", c, err)
	}
	t.Setenv("PROVIDER", "gemini")
	if _, err := Load(); err == nil {
		t.Fatal("unknown provider accepted")
	}
}
