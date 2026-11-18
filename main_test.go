package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/tools"
)

// L1: !key saves, lists and removes keys, swaps the owner tool, and never
// echoes the secret.
func TestKeyCommand(t *testing.T) {
	b := &bot{keysPath: filepath.Join(t.TempDir(), "api_keys.json"),
		newHTTPTool: func(k tools.Keys) llm.Tool {
			return llm.Tool{Name: "http_request", Description: strings.Join(k.Hosts(), ",")}
		}}
	const tok = "tok-secret-123456"
	steps := []struct{ args, want string }{
		{"", "No stored keys"},
		{"https://Tasks.Example.com/projects " + tok, "Saved a key for tasks.example.com (Bearer token)"},
		{"b.example.com X-Api-Key: " + tok, "Saved a key for b.example.com (header X-Api-Key)"},
		{"", "Stored keys for: b.example.com, tasks.example.com"},
		{"b.example.com remove", "Removed the key for b.example.com"},
		{"b.example.com remove", "No key stored"},
		{"localhost " + tok, "Usage"},
		{"tasks.example.com", "Usage"},
		{"tasks.example.com two words", "Usage"},
	}
	for _, s := range steps {
		got := b.keyCommand(strings.Fields(s.args))
		if !strings.Contains(got, s.want) || strings.Contains(got, tok) {
			t.Fatalf("!key %s = %q, want %q (and no secret)", s.args, got, s.want)
		}
	}
	k, err := tools.LoadKeys(b.keysPath)
	if err != nil || len(k) != 1 || k["tasks.example.com"]["Authorization"] != "Bearer "+tok {
		t.Fatalf("file: %v %v", k, err)
	}
	if d := b.httpOwner.Load().Description; d != "tasks.example.com" {
		t.Fatalf("owner tool not swapped: %q", d)
	}
}

// L13: !provider switches to a ready provider and lists them otherwise.
func TestProviderCommand(t *testing.T) {
	cfg := &config.Config{Provider: "anthropic", AnthropicAPIKey: "a", OpenRouterKey: "k", DataDir: t.TempDir(),
		Ready: map[string]config.ProviderConf{
			"anthropic":  {Model: "claude-haiku-5-5", Effort: "low"},
			"openrouter": {Model: "deepseek/deepseek-v4.1-flash-1017", Effort: "low"},
		}}
	b := &bot{cfg: cfg}
	b.models.Store(newModels(cfg, cfg.Provider, false))
	for _, tc := range []struct {
		args       []string
		want, kept string
	}{
		{nil, "Active provider: *anthropic*", "anthropic"},
		{[]string{"gemini"}, `Unknown provider "gemini".`, "anthropic"},
		{[]string{"openai"}, "openai isn't set up", "anthropic"},
		{[]string{"anthropic"}, "Already on anthropic.", "anthropic"},
		{[]string{"OpenRouter"}, "Switched to *openrouter*: deepseek/deepseek-v4.1-flash-1017", "openrouter"},
		{[]string{"openrouter", "x"}, "Active provider: *openrouter*", "openrouter"},
	} {
		got := b.providerCommand(tc.args)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%v: %q, want %q", tc.args, got, tc.want)
		}
		if p := b.models.Load().provider; p != tc.kept {
			t.Errorf("%v: provider %s, want %s", tc.args, p, tc.kept)
		}
	}
	if !b.replyLLM().SupportsTools() || b.curatorLLM() == nil {
		t.Fatal("clients not swapped")
	}
}
