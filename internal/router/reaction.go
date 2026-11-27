package router

import (
	"strings"
	"unicode"
)

// DefaultReaction answers a message the model left with an empty reply, so
// no addressed message is silently ignored (M7).
const DefaultReaction = "👍"

// ReactionOnly reports whether a reply is only emoji (M7) and returns the
// first one, sent as a reaction: WhatsApp takes one emoji per reaction. It
// reads whole sequences: keycaps (1️⃣), flags, skin tones, tag flags and
// joined ones (👨‍💻, 🙂‍↔️). Any letter, digit or punctuation outside one makes
// it a normal message.
func ReactionOnly(text string) (string, bool) {
	rs := []rune(strings.TrimSpace(text))
	if len(rs) == 0 {
		return "", false
	}
	var first string
	for i := 0; i < len(rs); {
		if unicode.IsSpace(rs[i]) {
			i++
			continue
		}
		n := emojiLen(rs[i:])
		if n == 0 {
			return "", false
		}
		if first == "" {
			first = string(rs[i : i+n])
		}
		i += n
	}
	return first, true
}

// emojiLen is the length in runes of the emoji sequence at the start of rs,
// or 0 if none starts there.
func emojiLen(rs []rune) int {
	r := rs[0]
	if r == '#' || r == '*' || (r >= '0' && r <= '9') { // keycap: 1, optional FE0F, 20E3
		j := 1
		if j < len(rs) && rs[j] == 0xFE0F {
			j++
		}
		if j < len(rs) && rs[j] == 0x20E3 {
			return j + 1
		}
		return 0
	}
	if !isEmoji(r) {
		return 0
	}
	j := 1
	if isRegional(r) && j < len(rs) && isRegional(rs[j]) { // a flag is a pair
		j++
	}
	for j < len(rs) {
		switch c := rs[j]; {
		case isModifier(c):
			j++
		case c == 0x200D && j+1 < len(rs) && isEmoji(rs[j+1]): // joined: 👨‍💻
			j += 2
		default:
			return j
		}
	}
	return j
}

func isRegional(r rune) bool { return r >= 0x1F1E6 && r <= 0x1F1FF }

// isModifier: variation selectors, skin tones, keycap and the tag characters
// of subdivision flags; each belongs to the emoji before it.
func isModifier(r rune) bool {
	return r == 0xFE0F || r == 0xFE0E || r == 0x20E3 || (r >= 0x1F3FB && r <= 0x1F3FF) || (r >= 0xE0020 && r <= 0xE007F)
}

func isEmoji(r rune) bool {
	switch {
	case r >= 0x1F000 && r <= 0x1FAFF, // pictographs, emoticons, flags, symbols
		r >= 0x2600 && r <= 0x27BF, // misc symbols, dingbats (❤ ✅ ✨)
		r >= 0x2190 && r <= 0x21FF, // arrows (↔ in 🙂‍↔️)
		r >= 0x2300 && r <= 0x23FF, // ⌚ ⏰
		r >= 0x25A0 && r <= 0x25FF, // ▶ ◻
		r >= 0x2900 && r <= 0x297F, // ⤴ ⤵
		r >= 0x2B00 && r <= 0x2BFF, // ⭐ ⬆
		r == 0x00A9, r == 0x00AE, r == 0x203C, r == 0x2049, r == 0x2122, r == 0x2139, r == 0x24C2, r == 0x3030, r == 0x303D, r == 0x3297, r == 0x3299:
		return true
	}
	return false
}
