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
	// 1F300–1FAFF is almost all emoji; elsewhere, only the code points
	// Unicode marks Emoji=Yes (emoji-data.txt), so plain arrows, shapes, chess
	// pieces and mahjong tiles ("←", "♜") stay text.
	if r >= 0x1F300 && r <= 0x1FAFF {
		return true
	}
	for _, rg := range emojiRanges {
		if r >= rg[0] && r <= rg[1] {
			return true
		}
	}
	return false
}

var emojiRanges = [][2]rune{
	{0x00A9, 0x00A9}, {0x00AE, 0x00AE}, {0x203C, 0x203C}, {0x2049, 0x2049},
	{0x2122, 0x2122}, {0x2139, 0x2139},
	{0x2194, 0x2199}, {0x21A9, 0x21AA}, // ↔ ↕ ↖ ↗ ↘ ↙ ↩ ↪
	{0x231A, 0x231B}, {0x2328, 0x2328}, {0x23CF, 0x23CF}, {0x23E9, 0x23F3}, {0x23F8, 0x23FA}, // ⌚ ⌨ ⏏ ⏩…⏳ ⏸ ⏹ ⏺
	{0x24C2, 0x24C2},
	{0x25AA, 0x25AB}, {0x25B6, 0x25B6}, {0x25C0, 0x25C0}, {0x25FB, 0x25FE}, // ▪ ▫ ▶ ◀ ◻…◾
	{0x2934, 0x2935},                                                       // ⤴ ⤵
	{0x2B05, 0x2B07}, {0x2B1B, 0x2B1C}, {0x2B50, 0x2B50}, {0x2B55, 0x2B55}, // ⬅ ⬆ ⬇ ⬛ ⬜ ⭐ ⭕
	{0x3030, 0x3030}, {0x303D, 0x303D}, {0x3297, 0x3297}, {0x3299, 0x3299},
	// Misc symbols and dingbats (☀ ☎ ✅ ❤ ✨), not ♜ ♖ or ☐.
	{0x2600, 0x2604}, {0x260E, 0x260E}, {0x2611, 0x2611}, {0x2614, 0x2615}, {0x2618, 0x2618},
	{0x261D, 0x261D}, {0x2620, 0x2620}, {0x2622, 0x2623}, {0x2626, 0x2626}, {0x262A, 0x262A},
	{0x262E, 0x262F}, {0x2638, 0x263A}, {0x2640, 0x2640}, {0x2642, 0x2642}, {0x2648, 0x2653},
	{0x265F, 0x2660}, {0x2663, 0x2663}, {0x2665, 0x2666}, {0x2668, 0x2668}, {0x267B, 0x267B},
	{0x267E, 0x267F}, {0x2692, 0x2697}, {0x2699, 0x2699}, {0x269B, 0x269C}, {0x26A0, 0x26A1},
	{0x26A7, 0x26A7}, {0x26AA, 0x26AB}, {0x26B0, 0x26B1}, {0x26BD, 0x26BE}, {0x26C4, 0x26C5},
	{0x26C8, 0x26C8}, {0x26CE, 0x26CF}, {0x26D1, 0x26D1}, {0x26D3, 0x26D4}, {0x26E9, 0x26EA},
	{0x26F0, 0x26F5}, {0x26F7, 0x26FA}, {0x26FD, 0x26FD}, {0x2702, 0x2702}, {0x2705, 0x2705},
	{0x2708, 0x270D}, {0x270F, 0x270F}, {0x2712, 0x2712}, {0x2714, 0x2714}, {0x2716, 0x2716},
	{0x271D, 0x271D}, {0x2721, 0x2721}, {0x2728, 0x2728}, {0x2733, 0x2734}, {0x2744, 0x2744},
	{0x2747, 0x2747}, {0x274C, 0x274C}, {0x274E, 0x274E}, {0x2753, 0x2755}, {0x2757, 0x2757},
	{0x2763, 0x2764}, {0x2795, 0x2797}, {0x27A1, 0x27A1}, {0x27B0, 0x27B0}, {0x27BF, 0x27BF},
	// 1F000–1F2FF: mahjong, cards, enclosed letters; only these are emoji.
	{0x1F004, 0x1F004}, {0x1F0CF, 0x1F0CF}, {0x1F170, 0x1F171}, {0x1F17E, 0x1F17F},
	{0x1F18E, 0x1F18E}, {0x1F191, 0x1F19A}, {0x1F1E6, 0x1F1FF}, {0x1F201, 0x1F202},
	{0x1F21A, 0x1F21A}, {0x1F22F, 0x1F22F}, {0x1F232, 0x1F23A}, {0x1F250, 0x1F251},
}
