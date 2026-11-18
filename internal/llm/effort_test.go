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
