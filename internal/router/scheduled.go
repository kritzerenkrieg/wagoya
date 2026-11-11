package router

import (
	"fmt"
	"strings"
	"time"
)

// ScheduledRequest is the request text for writing a due scheduled message
// (T1): the brief stored when it was scheduled and, for a recurring one, the
// texts sent on its last runs, so this run fits the moment and says
// something new. The run always writes a message; it can't stay silent.
func ScheduledRequest(brief, repeat string, sent []string) string {
	var b strings.Builder
	b.WriteString("[Scheduled message, due now] Write the message you scheduled for this moment, to send in this chat now. ")
	b.WriteString("It is not a reply to anyone: it is your own message, in character. ")
	fmt.Fprintf(&b, "Your note from when it was scheduled (what it is for, and who to tag): %s\n", oneLine(brief))
	b.WriteString("Fit it to the current time and to what is going on in the transcript, summary and people's current states; look things up with your tools first if that helps. ")
	b.WriteString("Always write the message, even if it seems moot: then adapt it. Keep the note's purpose and its tags (write them as @P2 handles). Reply only with the message itself.")
	if repeat != "" {
		fmt.Fprintf(&b, "\nIt repeats %s.", repeat)
		if len(sent) > 0 {
			b.WriteString(" What you sent on its last runs, newest first. Say it differently this time: don't reuse their wording, opener or joke.")
			for _, s := range sent {
				fmt.Fprintf(&b, "\n- %s", oneLine(capRunes(s, 300)))
			}
		}
	}
	return b.String()
}

// TaskReminderBrief is the brief for a task's reminder (U3), sent as written
// if the run can't write it fresh. In a DM it speaks to the owner; in a
// group it tags them by key (L10).
func TaskReminderBrief(name, desc string, due, now time.Time, dm bool, owner string) string {
	when := due.Format("Mon 2 Jan 15:04")
	day := func(t time.Time) string { return t.Format("2006-01-02") }
	switch day(due) {
	case day(now):
		when = "today at " + due.Format("15:04")
	case day(now.AddDate(0, 0, 1)):
		when = "tomorrow at " + due.Format("15:04")
	}
	name = strings.ReplaceAll(oneLine(name), "@{", "@ {")
	out := fmt.Sprintf("Reminder: your task \"%s\" is due %s.", capRunes(name, 200), when)
	if d := strings.ReplaceAll(oneLine(desc), "@{", "@ {"); d != "" {
		out += " (" + capRunes(d, 200) + ")"
	}
	if !dm {
		out = "@{" + owner + "} " + out
	}
	return out
}
