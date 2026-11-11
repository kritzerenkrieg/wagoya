package router

import (
	"strings"
	"testing"
	"time"
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

// U1, U2, U4: the requester's task counts per status and categories nudge Rin to list_tasks.
func TestBacklogCountInPeople(t *testing.T) {
	p := BuildPrompt(Prompt{People: []Person{{Handle: "P1", Name: "Ann", TaskCounts: map[string]int{"backlog": 9, "ongoing": 2}, TaskCats: []string{"work", "hobby"}}, {Handle: "P2", Name: "Bob"}}, Requester: Line{SenderName: "Ann", Handle: "P1"}, Text: "hi"})
	if !strings.Contains(p, "their tasks: 2 ongoing, 9 backlog (categories: work, hobby); use list_tasks") || strings.Count(p, "their tasks:") != 1 {
		t.Fatalf("prompt:\n%s", p)
	}
}

// U3: the fallback reminder speaks to the owner in a DM and tags them in a group.
func TestTaskReminderBrief(t *testing.T) {
	now := time.Date(2026, 11, 11, 9, 0, 0, 0, time.Local)
	due := time.Date(2026, 11, 12, 18, 0, 0, 0, time.Local)
	if got := TaskReminderBrief("Ship demo", "itch.io", due, now, true, "lid1"); got != `Reminder: your task "Ship demo" is due tomorrow at 18:00. (itch.io)` {
		t.Fatalf("dm: %q", got)
	}
	if got := TaskReminderBrief("Ship @{x}", "", due.Add(-24*time.Hour), now, false, "lid1"); got != `@{lid1} Reminder: your task "Ship @ {x}" is due today at 18:00.` {
		t.Fatalf("group: %q", got)
	}
}
