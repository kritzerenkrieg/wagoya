package router

import (
	"strings"
	"testing"
)

// T1: the run sees the brief, the repeat and earlier sends; a one-off has no
// earlier sends section.
func TestScheduledRequest(t *testing.T) {
	r := ScheduledRequest("lunch nag for @P2", "daily", []string{"eat, @Ana!", "food time"})
	for _, want := range []string{"lunch nag for @P2", "repeats daily", "- eat, @Ana!", "- food time", "Always write"} {
		if !strings.Contains(r, want) {
			t.Errorf("missing %q in %q", want, r)
		}
	}
	if one := ScheduledRequest("futsal at 19:00", "", nil); strings.Contains(one, "repeats") || strings.Contains(one, "last runs") {
		t.Errorf("one-off: %q", one)
	}
}

// T1: pinned tags show as this prompt's handles, else the person's name.
func TestUnpinMentions(t *testing.T) {
	handles := map[string]string{"P1": "lid1"}
	got := UnpinMentions("hey @{lid1} and @{pn:62} and @{gone}", handles, func(k string) string {
		if k == "pn:62" {
			return "Budi"
		}
		return ""
	})
	if got != "hey @P1 and @Budi and @someone" {
		t.Fatalf("got %q", got)
	}
}
