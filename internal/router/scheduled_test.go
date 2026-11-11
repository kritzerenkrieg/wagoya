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

// U1: the requester's open-task count nudges Rin to list_tasks.
func TestBacklogCountInPeople(t *testing.T) {
	p := BuildPrompt(Prompt{People: []Person{{Handle: "P1", Name: "Ann", OpenTasks: 3}, {Handle: "P2", Name: "Bob"}}, Requester: Line{SenderName: "Ann", Handle: "P1"}, Text: "hi"})
	if !strings.Contains(p, "backlog: 3 open tasks of theirs, use list_tasks") || strings.Count(p, "backlog:") != 1 {
		t.Fatalf("prompt:\n%s", p)
	}
}
