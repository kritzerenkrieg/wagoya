package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/tools"
	"go.mau.fi/whatsmeow/proto/waE2E"
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
	b.models.Store(newModels(cfg, cfg.Provider, cfg.Ready[cfg.Provider].Effort, false))
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

// Y1: !reasoning sets the active provider's effort only; each provider
// keeps its own across !provider switches.
func TestReasoningCommand(t *testing.T) {
	cfg := &config.Config{Provider: "anthropic", AnthropicAPIKey: "a", OpenRouterKey: "k", DataDir: t.TempDir(),
		Ready: map[string]config.ProviderConf{
			"anthropic":  {Model: "claude-haiku-5-5", Effort: "medium"},
			"openrouter": {Model: "deepseek/deepseek-v4.1-flash-1017", Effort: "none"},
		}}
	b := &bot{cfg: cfg}
	b.models.Store(newModels(cfg, cfg.Provider, cfg.Ready[cfg.Provider].Effort, false))
	effort := func() string { return b.models.Load().effort }
	for _, tc := range []struct {
		args       []string
		want, kept string
	}{
		{nil, "Reasoning on *anthropic*: medium", "medium"},
		{[]string{"max"}, `Unknown level "max".`, "medium"},
		{[]string{"low", "x"}, `Unknown level "low x".`, "medium"},
		{[]string{"medium"}, "Already at medium.", "medium"},
		{[]string{"HIGH"}, "Reasoning on *anthropic* is now high", "high"},
		{[]string{"none"}, "none (runs as low)", "none"},
	} {
		if got := b.reasoningCommand(tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%v: %q, want %q", tc.args, got, tc.want)
		}
		if effort() != tc.kept {
			t.Errorf("%v: effort %s, want %s", tc.args, effort(), tc.kept)
		}
	}
	// Switching provider uses that provider's own effort, not anthropic's.
	if got := b.providerCommand([]string{"openrouter"}); !strings.Contains(got, "effort none") || effort() != "none" {
		t.Fatalf("openrouter took another provider's effort: %q, %s", got, effort())
	}
	b.reasoningCommand([]string{"high"})
	if got := b.providerCommand([]string{"anthropic"}); !strings.Contains(got, "effort none") || effort() != "none" {
		t.Fatalf("anthropic lost its own effort: %q, %s", got, effort())
	}
	if list := b.providerCommand(nil); !strings.Contains(list, "openrouter: deepseek/deepseek-v4.1-flash-1017, effort high") {
		t.Errorf("list doesn't show openrouter's effort: %s", list)
	}
}

// D9: an over-long persona is recompressed whole, retried lower once, and
// rejected (old one kept) if it still doesn't fit; a short one is untouched.
func TestFitPersona(t *testing.T) {
	long := strings.Repeat("a", router.MaxPersonaRunes+10)
	short := "You are Bot."
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

// F5: a group's last photo is attachable for five minutes, per chat.
func TestRecentImage(t *testing.T) {
	b := &bot{recentImg: map[string]recentImage{}}
	t0 := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	img := &waE2E.ImageMessage{}
	b.noteImage("g1", img, "Jane", t0)
	if r, ok := b.lastImage("g1", t0.Add(4*time.Minute)); !ok || r.img != img || r.from != "Jane" {
		t.Fatalf("recent photo not found: %+v %v", r, ok)
	}
	if _, ok := b.lastImage("g2", t0.Add(time.Minute)); ok {
		t.Fatal("photo leaked to another chat")
	}
	if _, ok := b.lastImage("g1", t0.Add(6*time.Minute)); ok {
		t.Fatal("stale photo attached")
	}
	b.noteImage("g2", img, "Jim", t0.Add(10*time.Minute)) // prunes g1
	if len(b.recentImg) != 1 {
		t.Fatalf("stale photos kept: %d", len(b.recentImg))
	}
}
