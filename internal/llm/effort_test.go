package llm

import "testing"

// Claude has no "none": it falls back to "low"; OpenRouter passes it (L13).
func TestEffortMapping(t *testing.T) {
	for in, want := range map[string]string{"none": "low", "minimal": "low", "": "low", "low": "low", "medium": "medium", "max": "max"} {
		if got := claudeEffort(in); got != want {
			t.Errorf("claudeEffort(%q) = %q, want %q", in, got, want)
		}
	}
	if got := orEffort("none"); got != "none" {
		t.Errorf("orEffort(none) = %q", got)
	}
	if a := NewAPI("k", "m", "none").(*apiClient); a.effort != "low" {
		t.Errorf("API client effort %q", a.effort)
	}
	if c := NewCLI("claude", "t", "m", "none", t.TempDir(), false).(*cliClient); c.effort != "low" {
		t.Errorf("CLI client effort %q", c.effort)
	}
}

// Y1: EffectiveEffort reports what each client really sends.
func TestEffectiveEffort(t *testing.T) {
	for _, c := range []struct{ provider, in, want string }{
		{"anthropic", "none", "low"},
		{"anthropic", "", "low"},
		{"anthropic", "minimal", "low"},
		{"anthropic", "high", "high"},
		{"anthropic", "max", "max"},
		{"openrouter", "none", "none"},
		{"openrouter", "xhigh", "high"},
		{"openrouter", "max", "high"},
		{"openai", "medium", "medium"},
		{"openai", "bogus", "low"},
	} {
		if got := EffectiveEffort(c.provider, c.in); got != c.want {
			t.Errorf("EffectiveEffort(%q, %q) = %q, want %q", c.provider, c.in, got, c.want)
		}
		// The same mapping the clients use.
		var client string
		if c.provider == "anthropic" {
			client = claudeEffort(c.in)
		} else {
			client = orEffort(c.in)
		}
		if client != c.want {
			t.Errorf("%s client maps %q to %q", c.provider, c.in, client)
		}
	}
}
