package router

import (
	"fmt"
	"strings"
	"time"
)

// BuildPrompt renders the prompt. P4: what changes least comes first, so a
// provider's prompt cache can reuse it: the chat kind, the summary (one per
// curator pass) and the transcript (stable for a window block, P6), then
// what changes with every request: people, memories, reminders, the time.
func BuildPrompt(p Prompt) string {
	var b strings.Builder
	if p.Group {
		b.WriteString("<chat>group chat</chat>\n\n")
	} else {
		b.WriteString("<chat>private DM with the requester</chat>\n\n")
	}
	if s := strings.TrimSpace(p.Summary); s != "" {
		fmt.Fprintf(&b, "<summary>\n%s\n</summary>\n\n", clean(s))
	}
	b.WriteString("<transcript>\n")
	history := p.History
	if !p.KeepOwnLines {
		history = compactOwn(history) // Z1
	}
	for _, l := range history {
		b.WriteString(renderLine(l))
	}
	b.WriteString("</transcript>\n\n")
	if len(p.People) > 0 {
		b.WriteString("<people>\n")
		for _, pr := range p.People {
			fmt.Fprintf(&b, "%s %s (whitelisted=%q)", pr.Handle, nameOr(pr.Name), yesNo(pr.Whitelisted))
			switch {
			case len(pr.Facts) > 0:
				b.WriteString(":\n")
				for _, f := range pr.Facts {
					fmt.Fprintf(&b, "  - %s\n", clean(oneLine(f)))
				}
			case pr.HasProfile: // V1
				fmt.Fprintf(&b, ": a profile (who they are: read_profile) and %d stored facts (details: search_facts)\n", pr.FactCount)
			case pr.FactCount > 0:
				fmt.Fprintf(&b, ": %d stored facts, use search_facts\n", pr.FactCount)
			default:
				b.WriteString(": no known facts\n")
			}
			if len(pr.States) > 0 {
				b.WriteString("  current states (newest first; newer beats older and the summary; against a transcript line, the more recent wins):\n")
				for _, f := range pr.States {
					fmt.Fprintf(&b, "  - %s\n", clean(oneLine(f)))
				}
			}
			if c := taskCounts(pr.TaskCounts); c != "" {
				cats := ""
				if len(pr.TaskCats) > 0 {
					cats = " (categories: " + clean(oneLine(strings.Join(pr.TaskCats, ", "))) + ")"
				}
				fmt.Fprintf(&b, "  their tasks: %s%s; use list_tasks\n", c, cats)
			}
			if len(pr.Preferences) > 0 {
				b.WriteString("  standing preferences (they set these; they replace your default style for them):\n")
				for _, f := range pr.Preferences {
					fmt.Fprintf(&b, "  - %s\n", clean(oneLine(f)))
				}
			}
		}
		b.WriteString("</people>\n\n")
	}
	if len(p.Memories) > 0 {
		b.WriteString("<memories>\n")
		for _, m := range p.Memories {
			fmt.Fprintf(&b, "[%s] %s\n", m.Category, clean(oneLine(m.Text)))
		}
		b.WriteString("</memories>\n\n")
	}
	if len(p.Reminders) > 0 && !p.Now.IsZero() {
		b.WriteString("<reminders>\n")
		for i, r := range p.Reminders {
			if i == MaxReminders {
				break
			}
			rep := ""
			if r.Repeat != "" {
				rep = ", repeats " + r.Repeat
			}
			fmt.Fprintf(&b, "- %s (%s%s): %s\n", dayClock(r.At, p.Now), until(r.At.Sub(p.Now)), rep, clean(oneLine(capRunes(r.Text, 120))))
		}
		b.WriteString("</reminders>\n\n")
	}
	if !p.Now.IsZero() {
		fmt.Fprintf(&b, "<now>%s</now>\n\n", p.Now.Format("Monday 2006-01-02 15:04 MST"))
	}
	b.WriteString(reminder(Openers(p.History, RecentOpeners))) // X4: next to the request, where a long prompt keeps it
	img := ""
	if p.HasImage {
		img = ` image="attached"`
		if p.ImageNote != "" { // F5
			img = fmt.Sprintf(` image=%q`, "attached: "+clean(oneLine(p.ImageNote)))
		}
	}
	handle := ""
	if p.Requester.Handle != "" {
		handle = fmt.Sprintf(" id=%q", p.Requester.Handle)
	}
	fmt.Fprintf(&b, "<request from=%q%s whitelisted=%q%s>\n%s\n</request>",
		nameOr(p.Requester.SenderName), handle, yesNo(p.Requester.Whitelisted), img, clean(capRunes(p.Text, MaxRequestRunes)))
	return b.String()
}

func nameOr(name string) string {
	n := clean(oneLine(name))
	if n == "" {
		return "unknown"
	}
	return n
}

// addressees renders K7's to="…": "you" for the bot, then names.
func addressees(l Line) string {
	var to []string
	if l.ToBot && !l.FromBot {
		to = append(to, "you")
	}
	for _, n := range l.To {
		if n = strings.ReplaceAll(oneLine(clean(n)), `"`, "'"); n != "" && len(to) < 6 { // a name can't close the attribute
			to = append(to, n)
		}
	}
	return strings.Join(to, ", ")
}

func renderLine(l Line) string {
	who := nameOr(l.SenderName)
	attrs := ""
	switch {
	case l.FromBot:
		attrs = " (you)"
	case l.Handle != "":
		attrs = " [" + l.Handle + "]"
	}
	if l.Whitelisted && !l.FromBot {
		attrs += ` whitelisted="yes"`
	}
	if to := addressees(l); to != "" { // K7
		attrs += ` to="` + to + `"`
	}
	react := ""
	if len(l.Reactions) > 0 { // L6
		parts := make([]string, 0, len(l.Reactions))
		for _, r := range l.Reactions {
			parts = append(parts, nameOr(r.Name)+" "+clean(oneLine(capRunes(r.Emoji, 16))))
		}
		react = " (reactions: " + strings.Join(parts, ", ") + ")"
	}
	return fmt.Sprintf("[%s] %s%s: %s%s\n", l.Time.Format("2006-01-02 15:04"), who, attrs, clean(capRunes(l.Text, MaxLineRunes)), react)
}

// taskCounts renders "2 ongoing, 1 hold, 9 backlog" (U2), or "".
func taskCounts(c map[string]int) string {
	var parts []string
	for _, st := range []string{"ongoing", "hold", "backlog"} {
		if c[st] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", c[st], st))
		}
	}
	return strings.Join(parts, ", ")
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

// WithQuote prefixes text with the message it replies to, so Claude sees what
// "this" refers to (F1). The quote is cut to max runes.
func WithQuote(text, name, quote string, max int) string {
	quote = oneLine(quote)
	if quote == "" {
		return text
	}
	if r := []rune(quote); len(r) > max {
		quote = string(r[:max]) + "…"
	}
	if name = oneLine(name); name == "" {
		name = "someone"
	}
	q := fmt.Sprintf("(replying to %s: \"%s\")", name, quote)
	if text == "" {
		return q
	}
	return q + " " + text
}

// ImplicitQuote returns the bot's own message that a DM trigger answers when
// it quotes nothing and comes right after that message (F4): a bare "ye"
// answers the bot's last question, not something older. history is the
// window before the trigger, oldest first.
func ImplicitQuote(history []Line) (string, bool) {
	if len(history) == 0 {
		return "", false
	}
	last := history[len(history)-1]
	if !last.FromBot || strings.TrimSpace(last.Text) == "" {
		return "", false
	}
	return last.Text, true
}

// dayClock is t as "18:45 today", "07:00 tomorrow" or "Sat 2026-11-07 07:00".
func dayClock(t, now time.Time) string {
	y1, m1, d1 := t.Date()
	y2, m2, d2 := now.Date()
	switch {
	case y1 == y2 && m1 == m2 && d1 == d2:
		return t.Format("15:04") + " today"
	case t.Sub(now) < 48*time.Hour && now.AddDate(0, 0, 1).Day() == d1:
		return t.Format("15:04") + " tomorrow"
	}
	return t.Format("Mon 2006-01-02 15:04")
}

// until is a duration as "in 1h35m", "in 12m" or "due now".
func until(d time.Duration) string {
	m := int(d.Round(time.Minute).Minutes())
	switch {
	case m <= 0:
		return "due now"
	case m < 60:
		return fmt.Sprintf("in %dm", m)
	case m%60 == 0:
		return fmt.Sprintf("in %dh", m/60)
	}
	return fmt.Sprintf("in %dh%02dm", m/60, m%60)
}
