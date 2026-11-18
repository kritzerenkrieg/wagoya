package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
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

// D9: an over-long persona is recompressed whole, retried lower once, and
// rejected (old one kept) if it still doesn't fit; a short one is untouched.
func TestFitPersona(t *testing.T) {
	long := strings.Repeat("a", router.MaxPersonaRunes+10)
	short := "You are Rin."
	var limits []string
	ask := func(outs ...string) func(string, string) (string, error) {
		return func(system, prompt string) (string, error) {
			limits = append(limits, strings.SplitN(prompt, "\n", 2)[0])
			out := outs[0]
			outs = outs[1:]
			return out, nil
		}
	}
	if got, err := fitPersona(short, ask()); err != nil || got != short {
		t.Fatalf("short persona: %q %v", got, err)
	}
	if got, err := fitPersona(long, ask("<persona>"+short+"</persona>")); err != nil || got != short {
		t.Fatalf("recompressed: %q %v", got, err)
	}
	limits = nil
	if got, err := fitPersona(long, ask(long, short)); err != nil || got != short || len(limits) != 2 || !strings.Contains(limits[1], fmt.Sprint(router.MaxPersonaRunes-200)) {
		t.Fatalf("second try: %q %v %v", got, err, limits)
	}
	if _, err := fitPersona(long, ask(long, long)); err == nil {
		t.Fatal("still too long was accepted")
	}
	if _, err := fitPersona(long, func(string, string) (string, error) { return "", fmt.Errorf("down") }); err == nil {
		t.Fatal("model error was accepted")
	}
}
