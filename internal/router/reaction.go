package router

import (
	"strings"
	"unicode"
)

// DefaultReaction answers a message the model left with an empty reply, so
// no addressed message is silently ignored (M7).
const DefaultReaction = "👍"

// ReactionOnly reports whether a reply is only emoji (M7) and returns the
// first one, sent as a reaction: WhatsApp takes one emoji per reaction. Any
// letter, digit or punctuation makes it a normal message.
func ReactionOnly(text string) (string, bool) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", false
	}
	var first []rune
	done, regional := false, 0
	for _, r := range []rune(text) {
		switch {
		case unicode.IsSpace(r):
			if len(first) > 0 {
				done = true
			}
			continue
		case r == 0x200D || r == 0xFE0F || r == 0xFE0E || (r >= 0x1F3FB && r <= 0x1F3FF) || r == 0x20E3:
			// joiner, variation selectors, skin tones, keycap: part of the
			// emoji before them.
			if len(first) == 0 {
				return "", false
			}
			if !done {
				first = append(first, r)
			}
			continue
		case isEmoji(r):
		default:
			return "", false
		}
		if done {
			continue
		}
		switch {
		case len(first) == 0:
		case first[len(first)-1] == 0x200D: // a ZWJ sequence goes on
		case r >= 0x1F1E6 && r <= 0x1F1FF && regional == 1: // second half of a flag
		default:
			done = true
			continue
		}
		if r >= 0x1F1E6 && r <= 0x1F1FF {
			regional++
		}
		first = append(first, r)
	}
	return string(first), len(first) > 0
}

func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, flags, symbols
		r >= 0x2600 && r <= 0x27BF, // misc symbols, dingbats (❤ ✅ ✨)
		r >= 0x2300 && r <= 0x23FF, // ⌚ ⏰
		r >= 0x2B00 && r <= 0x2BFF, // ⭐ ⬆
		r == 0x2764, r == 0x00A9, r == 0x00AE, r == 0x203C, r == 0x2049, r == 0x2122, r == 0x3030, r == 0x303D:
		return true
	}
	return false
}
