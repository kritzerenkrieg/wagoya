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
		"!reasoning":                 "reasoning|",
		"@Bot !Reasoning HIGH":       "reasoning|HIGH",
		"!time +7":                   "time|+7",
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
	for _, c := range []string{"!help", "!budget", "!provider", "!reasoning", "!reset", "!mute", "!unmute", "!key"} {
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
		{Name: "anthropic", Model: "claude-haiku-5-5", Effort: Effort{"none", "low"}},
		{Name: "openrouter"},
		{Name: "openai", Model: "abliterated-model", Effort: Effort{"none", "none"}},
	}, `Unknown provider "gpt".`)
	for _, want := range []string{`Unknown provider "gpt".`, "Active provider: *openai*", "- anthropic: claude-haiku-5-5, effort none (runs as low)\n",
		"- openrouter: not set up", "- openai: abliterated-model, effort none (active)", "Usage: !provider anthropic"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
}

// Y1: the effort shown, Claude's fallback for none, the default after a change.
func TestReasoningText(t *testing.T) {
	got := ReasoningText("anthropic", Effort{"none", "low"}, "medium", `Unknown level "max".`)
	for _, want := range []string{`Unknown level "max".`, "Reasoning on *anthropic*: none (runs as low) (default medium)", "Levels: none, low, medium, high", "Usage: !reasoning medium"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	if got := ReasoningText("openrouter", Effort{"none", "none"}, "none", ""); !strings.Contains(got, "*openrouter*: none\n") || strings.Contains(got, "default") {
		t.Errorf("openrouter none: %s", got)
	}
	// Any setting that runs differently shows what it really runs at.
	if got := ReasoningText("openrouter", Effort{"xhigh", "high"}, "xhigh", ""); !strings.Contains(got, "*openrouter*: xhigh (runs as high)\n") {
		t.Errorf("openrouter xhigh: %s", got)
	}
	if got := ReasoningSet("openai", Effort{"high", "high"}); !strings.Contains(got, "*openai* is now high,") || !strings.Contains(got, "stays after a restart") {
		t.Errorf("set: %s", got)
	}
}

func TestWebCommand(t *testing.T) {
	if c, ok := ParseCommand("@Bot !web openrouter"); !ok || c.Name != "web" || len(c.Args) != 1 {
		t.Fatalf("parse: %+v %v", c, ok)
	}
	out := WebText("builtin", []WebBackendInfo{{"builtin", true}, {"openrouter", false}}, 64000, 8000, "openrouter: no api key set.")
	for _, want := range []string{"openrouter: no api key set.", "*builtin*", "- builtin: ready (active)", "- openrouter: no api key set", "64k tokens of pages, digested to 8k"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
}

// L15: !time takes hours from UTC, with or without a sign or minutes.
func TestParseUTCOffset(t *testing.T) {
	for in, want := range map[string]string{
		"+7": "UTC+7", "7": "UTC+7", "UTC+7": "UTC+7", "utc-3": "UTC-3", "-3": "UTC-3", "0": "UTC", "UTC": "UTC",
		"+5:30": "UTC+5:30", "+0545": "UTC+5:45", "+14": "UTC+14", "-12": "UTC-12",
		"+15": "", "-13": "", "+5:20": "", "seven": "", "+": "", "++7": "", "+7:300": "", "Asia/Jakarta": "",
	} {
		off, ok := ParseUTCOffset(in)
		got := ""
		if ok {
			got = ZoneName(off)
		}
		if got != want {
			t.Errorf("ParseUTCOffset(%q) = %q, want %q", in, got, want)
		}
	}
}
