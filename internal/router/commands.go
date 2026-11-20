package router

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Command is an owner command handled by the bot itself, never by the model
// and never stored (L1, L2, M2, L12). Commands start with "!" (L12), so they
// don't clash with other bots' "/" commands in the same group.
type Command struct {
	Name string   // "key", "reset", "mute", "unmute", "budget", "provider", "reasoning", "web", "time" or "help"
	Args []string // the words after the name
}

// ParseCommand recognizes !key, !reset, !mute, !unmute, !budget, !provider, !reasoning, !web, !time and !help as the
// first word, after any leading @mentions ("@<BOT_NAME> !reset"). ok is false for
// anything else, "/reset" included (L12).
func ParseCommand(text string) (Command, bool) {
	words := strings.Fields(text)
	for len(words) > 0 && strings.HasPrefix(words[0], "@") {
		words = words[1:]
	}
	if len(words) == 0 {
		return Command{}, false
	}
	switch name := strings.ToLower(words[0]); name {
	case "!key", "!reset", "!mute", "!unmute", "!budget", "!provider", "!reasoning", "!web", "!time", "!help":
		return Command{Name: name[1:], Args: words[1:]}, true
	}
	return Command{}, false
}

// ParseMuteDuration reads a !mute duration such as "30m", "2h" or "1d" (M2).
func ParseMuteDuration(s string) (time.Duration, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 || n > 10000 {
		return 0, false
	}
	switch s[len(s)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}

// BudgetText is the !budget reply (L12): today's spend against the cap (M3).
func BudgetText(spent, limit float64) string {
	pct := 0.0
	if limit > 0 {
		pct = spent / limit * 100
	}
	s := fmt.Sprintf("Today's spend: $%.4f of $%.2f (%.0f%%). It resets at midnight.", spent, limit, pct)
	if spent >= limit {
		s += "\nOver the cap: I'm only answering my owner until then."
	}
	return s
}

// HelpText is the !help reply (L12): every owner command.
const HelpText = "Owner commands:\n" +
	"- !help: this list\n" +
	"- !budget: today's spend against the daily cap\n" +
	"- !provider [name]: show the model provider, or switch it (until restart)\n" +
	"- !reasoning [none|low|medium|high]: show or set the active provider's reasoning effort (saved)\n" +
	"- !web [builtin|openrouter]: show the web search backend, or switch it (until restart)\n" +
	"- !time [+7]: show the time zone, or set it as hours from UTC (saved; the bot restarts itself)\n" +
	"- !reset: clear this chat's context (facts, memories and reminders are kept)\n" +
	"- !mute @person [30m|2h|1d]: mute someone in every chat, until !unmute or for a while\n" +
	"- !unmute @person: lift a mute or a flood timeout\n" +
	"- !mute: list who is muted\n" +
	"- !key: stored API keys (DM only; send !key for details)"

// ProviderInfo is one provider in the !provider list (L13). Model is empty
// when its credentials aren't set.
type ProviderInfo struct {
	Name, Model string
	Effort      Effort // Y1: shown with what it really runs at
}

// ProviderText is the !provider reply for no argument or a wrong one (L13):
// the active provider, every provider, and the usage. note, if set, says
// what was wrong.
func ProviderText(active string, list []ProviderInfo, note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n\n")
	}
	b.WriteString("Active provider: *" + active + "*\n\nProviders:\n")
	for _, p := range list {
		mark := ""
		if p.Name == active {
			mark = " (active)"
		}
		if p.Model == "" {
			fmt.Fprintf(&b, "- %s: not set up (no key in .env)\n", p.Name)
		} else {
			fmt.Fprintf(&b, "- %s: %s, effort %s%s\n", p.Name, p.Model, p.Effort, mark)
		}
	}
	b.WriteString("\nUsage: !provider anthropic")
	return b.String()
}

// Efforts are the reasoning levels !reasoning takes (Y1).
var Efforts = []string{"none", "low", "medium", "high"}

// Effort is a provider's effort setting and what its client really runs at
// (llm.EffectiveEffort): Claude has no "none" or "minimal", and OpenRouter
// takes nothing above "high" (L13).
type Effort struct{ Set, Runs string }

// String is the effort as shown to the owner, with any fallback spelled out.
func (e Effort) String() string {
	if e.Runs != "" && e.Runs != e.Set {
		return e.Set + " (runs as " + e.Runs + ")"
	}
	return e.Set
}

// ReasoningText is the !reasoning reply for no argument or a wrong one (Y1):
// the active provider's effort, the default if it differs, and the usage.
func ReasoningText(provider string, effort Effort, def, note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n\n")
	}
	fmt.Fprintf(&b, "Reasoning on *%s*: %s", provider, effort)
	if effort.Set != def {
		fmt.Fprintf(&b, " (default %s)", def)
	}
	b.WriteString("\n\nLevels: " + strings.Join(Efforts, ", ") + ". none turns reasoning off where the provider allows it; Claude runs it as low.")
	b.WriteString("\nUsage: !reasoning medium")
	return b.String()
}

// ReasoningSet is the !reasoning reply after a change (Y1).
func ReasoningSet(provider string, effort Effort) string {
	return fmt.Sprintf("Reasoning on *%s* is now %s, for replies and the curator.\nSaved: it stays after a restart.", provider, effort)
}

// WebBackendInfo is one backend in the !web list (W8).
type WebBackendInfo struct {
	Name  string
	Ready bool // its API key is set
}

// WebText is the !web reply for no argument or a wrong one (W8): the active
// backend, every backend, the deep search limits and the usage.
func WebText(active string, list []WebBackendInfo, raw, digest int, note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n\n")
	}
	b.WriteString("Web search backend: *" + active + "*\n\nBackends:\n")
	for _, w := range list {
		state := "ready"
		if !w.Ready {
			state = "no api key set"
		}
		if w.Name == active {
			state += " (active)"
		}
		fmt.Fprintf(&b, "- %s: %s\n", w.Name, state)
	}
	if digest > 0 {
		fmt.Fprintf(&b, "\nDeep search: up to %dk tokens of pages, digested to %dk.\n", raw/1000, digest/1000)
	} else {
		b.WriteString("\nDeep search: off (snippets only).\n")
	}
	b.WriteString("\nUsage: !web openrouter")
	return b.String()
}

// SearchingText is sent, quoting the request, when a reply starts its first
// deep web search (W9); the answer follows as its own message.
const SearchingText = "Searching..."

// ParseUTCOffset reads a !time offset (L15): "+7", "-3", "7", "+5:30",
// "+0545", "UTC+7" or "0". It returns seconds east of UTC, within the
// zones in use (-12 to +14, minutes 0, 30 or 45).
func ParseUTCOffset(s string) (int, bool) {
	s = strings.ToUpper(strings.TrimSpace(s))
	s = strings.TrimPrefix(strings.TrimPrefix(s, "UTC"), "GMT")
	if s == "" {
		return 0, true // "UTC" alone
	}
	sign := 1
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		sign, s = -1, s[1:]
	}
	h, m := s, "0"
	if i := strings.IndexByte(s, ':'); i >= 0 {
		h, m = s[:i], s[i+1:]
	} else if len(s) == 4 {
		h, m = s[:2], s[2:]
	}
	if strings.Trim(h+m, "0123456789") != "" {
		return 0, false // Atoi would take a second sign
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || len(h) == 0 || len(h) > 2 || len(m) > 2 || hh < 0 || (mm != 0 && mm != 30 && mm != 45) {
		return 0, false
	}
	off := sign * (hh*3600 + mm*60)
	if off < -12*3600 || off > 14*3600 {
		return 0, false
	}
	return off, true
}

// ZoneName is an offset as "UTC", "UTC+7", "UTC-3" or "UTC+5:30" (L15).
func ZoneName(off int) string {
	if off == 0 {
		return "UTC"
	}
	sign := "+"
	if off < 0 {
		sign, off = "-", -off
	}
	if m := off % 3600 / 60; m != 0 {
		return fmt.Sprintf("UTC%s%d:%02d", sign, off/3600, m)
	}
	return fmt.Sprintf("UTC%s%d", sign, off/3600)
}

// TimeText is the !time reply for no argument or a wrong one (L15).
func TimeText(now time.Time, note string) string {
	var b strings.Builder
	if note != "" {
		b.WriteString(note + "\n\n")
	}
	fmt.Fprintf(&b, "Time zone: *%s* (now %s)", now.Location(), now.Format("Mon 2006-01-02 15:04"))
	b.WriteString("\n\nUsage: !time +7 (hours from UTC, like -3 or +5:30)")
	return b.String()
}

// TimeSet is the !time reply after a change (L15).
func TimeSet(zone string) string {
	return "Time zone set to *" + zone + "*. Saved; restarting to apply it, back in a few seconds."
}
