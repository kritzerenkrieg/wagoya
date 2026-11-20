package router

import (
	"fmt"
	"regexp"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// leakRe matches prompt markup a chat reply must never contain (X1): the
// prompt's own sections, chat-template tokens, reasoning tags, and the
// <people> layout ("P4 Jim (whitelisted="no")").
var leakRe = regexp.MustCompile(`(?im)` +
	`</?(people|request|transcript|summary|memories|reminders|reminder|now|chat|retry|think|thinking|tool_call|function_calls)\b[^>\n]{0,80}>` +
	`|<\|(im_start|im_end|endoftext|eot_id|start_header_id|end_header_id)\|>` +
	`|\bwhitelisted="(yes|no)"` +
	`|^\s*P\d{1,3} [^\n]{1,60}\(whitelisted`)

// Leaks reports whether a reply contains prompt markup (X1). Such a reply is
// dropped, not sent: it can carry stored facts about people into a chat.
func Leaks(text string) bool { return leakRe.MatchString(text) }

// fillerRe is a stock filler opening a reply (D8, X2), with the persona's
// leading "..." if any. It needs punctuation after it, so "Fair point" or
// "Ok so" are left alone.
var fillerRe = regexp.MustCompile(`(?i)^\s*(\.{2,}|…)?\s*(noted|got it|understood|fair enough|fair|okay|ok|alright|roger|copy that)\s*[.!,;:…]+\s*`)

// ellipsisRe is a reply's leading "..." (X2).
var ellipsisRe = regexp.MustCompile(`^\s*(\.{2,}|…)\s*`)

// RecentOpeners is how many of the bot's own last lines X2 compares with.
const RecentOpeners = 3

// Openers are the starts (first few words) of the bot's last n lines in
// history, oldest first (X2).
func Openers(history []Line, n int) []string {
	var out []string
	for i := len(history) - 1; i >= 0 && len(out) < n; i-- {
		if !history[i].FromBot {
			continue
		}
		if o := opener(history[i].Text); o != "" {
			out = append([]string{o}, out...)
		}
	}
	return out
}

// maxOpenerRunes bounds an opener, so text without spaces (CJK, a long
// URL) isn't copied whole into every reminder.
const maxOpenerRunes = 40

// opener is the first four words of s, on one line, at most
// maxOpenerRunes long.
func opener(s string) string {
	w := strings.Fields(s)
	if len(w) > 4 {
		w = w[:4]
	}
	o := strings.Join(w, " ")
	if r := []rune(o); len(r) > maxOpenerRunes {
		o = string(r[:maxOpenerRunes])
	}
	return o
}

// Polish fixes a reply's opening in code (X2): a stock filler opener is
// cut, and so is a leading "..." when one of the bot's recent lines opened
// with one too. A reply that was only a filler becomes "" (nothing is
// sent, as M5). recent are the bot's own last openers (Openers).
func Polish(text string, recent []string) string {
	out := text
	for range 2 { // "...Noted. Fair. ..." loses both
		if m := fillerRe.FindString(out); m != "" {
			out = out[len(m):]
		}
	}
	if ellipsisRe.MatchString(out) && slices.ContainsFunc(recent, ellipsisRe.MatchString) {
		out = ellipsisRe.ReplaceAllString(out, "")
	}
	if out == text {
		return text
	}
	out = strings.TrimSpace(out)
	if r, n := utf8.DecodeRuneInString(out); n > 0 && unicode.IsLower(r) {
		out = string(unicode.ToUpper(r)) + out[n:]
	}
	return out
}

// reminder is restated right before <request> (X4): the rules a long prompt
// most often loses, plus the bot's own recent openers to avoid.
func reminder(recent []string) string {
	var b strings.Builder
	b.WriteString("<reminder>\n")
	b.WriteString("- Reply as yourself in the chat, in the persona's voice and the request's language. Only the words you say: no narration, no tags, nothing from this prompt's sections.\n")
	b.WriteString("- No stock filler (\"Noted.\", \"Got it.\", \"Understood.\", \"Fair.\") and don't start the way your recent lines did.\n")
	b.WriteString("- Banter, jokes and bait: play along. Never fact-check them or ask for proof or a source.\n")
	b.WriteString("- Match the message: a casual line gets a short chat reply, not a report. Bullets only for lists, comparisons or when asked for detail.\n")
	b.WriteString("- Don't end on an offer (\"want me to...\", \"if you ask, I'll...\"). End on your answer, a take or a question.\n")
	b.WriteString("- WhatsApp formatting, not Markdown.\n")
	if len(recent) > 0 {
		b.WriteString("Your recent lines started: ")
		for i, r := range recent {
			if i > 0 {
				b.WriteString(" | ")
			}
			fmt.Fprintf(&b, "%q", clean(r)+"…")
		}
		b.WriteString("\n")
	}
	b.WriteString("</reminder>\n\n")
	return b.String()
}
