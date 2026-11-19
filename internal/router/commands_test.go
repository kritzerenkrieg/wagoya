package router

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	for in, want := range map[string]string{
		"!reset":                     "reset|",
		"  !RESET  ":                 "reset|",
		"@Bot !reset":                "reset|",
		"!key":                       "key|",
		"!key tasks.example.com abc": "key|tasks.example.com abc",
		"!key h X-Api-Key: v":        "key|h X-Api-Key: v",
		"!budget":                    "budget|",
		"@Bot !help":                 "help|",
		"!provider":                  "provider|",
		"!Provider openrouter":       "provider|openrouter",
		"/reset":                     "", // L12: "/" is other bots' prefix now
		"/budget":                    "",
		"!keys":                      "",
		"please !reset":              "",
		"reset":                      "",
		"!resetting the chat":        "",
		"":                           "",
	} {
		c, ok := ParseCommand(in)
		got := ""
		if ok {
			got = c.Name + "|" + strings.Join(c.Args, " ")
		}
		if got != want {
			t.Errorf("ParseCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMuteCommands(t *testing.T) {
	for in, want := range map[string]string{
		"!mute @6281 30m": "mute [@6281 30m]",
		"@Bot !unmute @x": "unmute [@x]",
		"!MUTE":           "mute []",
	} {
		c, ok := ParseCommand(in)
		if got := c.Name + " " + fmt.Sprint(c.Args); !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, ok, want)
		}
	}
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "2H": 2 * time.Hour, "1d": 24 * time.Hour} {
		if d, ok := ParseMuteDuration(in); !ok || d != want {
			t.Errorf("%q: %v %v", in, d, ok)
		}
	}
	for _, in := range []string{"", "m", "0m", "-1h", "5x", "@x", "1.5h"} {
		if _, ok := ParseMuteDuration(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
	for in, want := range map[string]bool{"SPAM": true, "spam.": true, "OK": false, "ok": false, "**OK**": false, "": true, "maybe": true} {
		if IsSpam(in) != want {
			t.Errorf("IsSpam(%q) != %v", in, want)
		}
	}
}

// L12: !budget shows today's spend against the cap.
func TestBudgetText(t *testing.T) {
	if got := BudgetText(0.5, 2); got != "Today's spend: $0.5000 of $2.00 (25%). It resets at midnight." {
		t.Fatalf("under: %q", got)
	}
	if got := BudgetText(2.06, 2); !strings.Contains(got, "(103%)") || !strings.Contains(got, "only answering my owner") {
		t.Fatalf("over: %q", got)
	}
}

// L12: !help lists every command, all with the "!" prefix.
func TestHelpText(t *testing.T) {
	for _, c := range []string{"!help", "!budget", "!provider", "!reset", "!mute", "!unmute", "!key"} {
		if !strings.Contains(HelpText, c) {
			t.Errorf("help misses %s", c)
		}
	}
	if strings.Contains(HelpText, "/") {
		t.Errorf("help still has a / command: %s", HelpText)
	}
}

// L13: the list shows the active provider and which ones aren't set up.
func TestProviderText(t *testing.T) {
	got := ProviderText("openai", []ProviderInfo{
		{Name: "anthropic", Model: "claude-haiku-5-5", Effort: "low"},
		{Name: "openrouter"},
		{Name: "openai", Model: "abliterated-model", Effort: "none"},
	}, `Unknown provider "gpt".`)
	for _, want := range []string{`Unknown provider "gpt".`, "Active provider: *openai*", "- anthropic: claude-haiku-5-5, effort low\n",
		"- openrouter: not set up", "- openai: abliterated-model, effort none (active)", "Usage: !provider anthropic"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}
