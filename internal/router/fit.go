package router

import (
	"math"
	"strings"
	"unicode/utf8"
)

// ImageTokens is the budget reserved for one attached image. Claude images
// cost roughly width*height/750 tokens; this covers large ones.
const ImageTokens = 5000

// EstimateTokens is a deliberately high token estimate with no tokenizer:
// one token per 3 UTF-8 bytes. ASCII text averages about 4 bytes per token,
// and multi-byte scripts and emoji take more bytes, so this overestimates.
func EstimateTokens(s string) int { return (len(s) + 2) / 3 }

const truncNote = "\n[message truncated to fit the context limit]"

// FitPrompt is FitPromptWith at the default 3 bytes per token and no tools.
func FitPrompt(system string, p Prompt, budget int) string {
	return FitPromptWith(system, p, budget, DefaultBytesPerToken, 0)
}

// Caps on single items, so one giant paste can't push everything else out
// (J5).
const (
	MaxLineRunes    = 2000  // a transcript line
	MaxRequestRunes = 16000 // the request text
	keepMemories    = 3     // memories trimmed last, with the request
)

// FitPromptWith builds the prompt and keeps system + prompt + image + tool
// definitions (extraBytes) within budget tokens, estimating bpt bytes per
// token (B17, J5). It drops the oldest transcript lines first, then the
// memories beyond the top 3, then the summary, then profiles from the end
// (the requester's is first, so it stays), then the last memories, and only
// then truncates the request text (D5).
func FitPromptWith(system string, p Prompt, budget int, bpt float64, extraBytes int) string {
	if bpt <= 0 {
		bpt = DefaultBytesPerToken
	}
	est := func(s string) int { return int(math.Ceil(float64(len(s)) / bpt)) }
	fixed := est(system) + int(float64(extraBytes)/bpt)
	if p.HasImage {
		fixed += ImageTokens
	}
	for {
		s := BuildPrompt(p)
		over := fixed + est(s) - budget
		switch {
		case over <= 0:
			return s
		case len(p.History) > 0:
			p.History = p.History[1:]
		case len(p.Memories) > keepMemories:
			p.Memories = p.Memories[:len(p.Memories)-1] // search results are best-first
		case p.Summary != "":
			p.Summary = ""
		case len(p.People) > 1:
			p.People = p.People[:len(p.People)-1]
		case len(p.Memories) > 0:
			p.Memories = p.Memories[:len(p.Memories)-1]
		case len(p.People) > 0:
			p.People = nil
		default:
			// Loop again after cutting: escaping in BuildPrompt can grow text.
			keep := len(p.Text) - int(float64(over)*bpt) - len(truncNote)
			if keep <= 0 {
				p.Text = truncNote
				return BuildPrompt(p)
			}
			for keep > 0 && !utf8.RuneStart(p.Text[keep]) {
				keep-- // don't split a UTF-8 character
			}
			p.Text = strings.TrimSuffix(p.Text[:keep], truncNote) + truncNote
		}
	}
}

// capRunes shortens s to at most n runes, marking the cut.
func capRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n]) + truncNote
}
