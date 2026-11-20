package llm

import "testing"

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
