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

// Polish fixes a reply's opening in code (X2): a leading "..." is cut when
// one of the bot's recent lines opened with one too. Stock fillers are left
// to the prompt's style rule, which the persona may override (Z9, Z12).
// recent are the bot's own last openers (Openers).
func Polish(text string, recent []string) string {
	out := text
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
func reminder(recent []string, ownPhoto bool) string {
	var b strings.Builder
	b.WriteString("<reminder>\n")
	b.WriteString("- Reply as yourself in the chat, in the persona's voice and the request's language. Only the words you say: no narration and nothing copied from this prompt's sections (a hidden tag your rules ask for, like the persona tag, is fine).\n")
	b.WriteString("- No stock filler (\"Noted.\", \"Got it.\", \"Understood.\", \"Fair.\") and don't start the way your recent lines did.\n")
	b.WriteString("- Banter, jokes and bait: play along. Never fact-check them or ask for proof or a source.\n")
	b.WriteString("- Match the message: a casual line gets a short chat reply, not a report. Bullets only for lists, comparisons and steps, or when asked for detail.\n")
	b.WriteString("- Don't end on an offer (\"want me to...\", \"if you ask, I'll...\"). End on your answer, a take or a question.\n")
	b.WriteString("- WhatsApp formatting, not Markdown.\n")
	b.WriteString("- Language, length and bullets are your default style: the persona's voice wins over them, and a person's standing preferences replace them.\n") // Z9
	if ownPhoto {                                                                                                                                                   // F7
		b.WriteString("- The request comes with its own photo: it is about that photo. Start by saying what the photo shows; \"this\" means the photo, and any quoted message is only background.\n")
	}
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

// FullBotLines is how many of the bot's own latest transcript lines keep
// their full text; older ones are cut to OldBotRunes (Z1). Most of a window
// was the bot's own long answers, and the model copied their report style.
const (
	FullBotLines = 2
	OldBotRunes  = 280
)

// shortNote ends a shortened bot line (Z1).
const shortNote = " …(shortened; search chat_history for its words to read more)"

// compactOwn returns history with the bot's own lines older than its last
// FullBotLines cut to about OldBotRunes, at a word boundary (Z1). history
// itself is not changed.
func compactOwn(history []Line) []Line {
	var out []Line
	seen := 0
	for i := len(history) - 1; i >= 0; i-- {
		if !history[i].FromBot {
			continue
		}
		if seen++; seen <= FullBotLines {
			continue
		}
		r := []rune(history[i].Text)
		if len(r) <= OldBotRunes {
			continue
		}
		cut := string(r[:OldBotRunes])
		if j := strings.LastIndexAny(cut, " \n"); j > OldBotRunes/2 {
			cut = cut[:j]
		}
		if out == nil {
			out = append([]Line(nil), history...)
		}
		out[i].Text = strings.TrimRight(cut, " \n.,;:") + shortNote
	}
	if out == nil {
		return history
	}
	return out
}

// ActiveKeys are the person keys of the requester and of everyone who spoke
// in history: the people whose states go in the prompt (K4, Z2).
func ActiveKeys(handles map[string]string, history []Line, req Line) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range append([]Line{req}, history...) {
		if k := handles[l.Handle]; k != "" && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}
