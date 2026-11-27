package router

import "testing"

// M7: an emoji-only reply becomes a reaction with its first emoji; anything
// with words stays a message.
func TestReactionOnly(t *testing.T) {
	for in, want := range map[string]string{
		"👍":       "👍",
		" ❤️ ":    "❤️",
		"😂😂":      "😂",
		"🙏 🔥":     "🙏",
		"👍🏽":      "👍🏽",
		"👨‍💻":     "👨‍💻",
		"🇮🇩":      "🇮🇩",
		"🇮🇩🇯🇵":    "🇮🇩",
		"ok 👍":    "",
		"👍!":      "",
		"lol":     "",
		"":        "",
		"5":       "",
		"1️⃣":     "1️⃣",
		"#⃣":      "#⃣",
		"🙂‍↔️":    "🙂‍↔️",
		"👩🏽‍💻 🔥":  "👩🏽‍💻",
		"🏴󠁧󠁢󠁳󠁣󠁴󠁿": "🏴󠁧󠁢󠁳󠁣󠁴󠁿",
		"1️":      "",
		"👍 5":     "",
		"← →":     "",
		"▲":       "",
		"⬅️":      "⬅️",
		"⭐":       "⭐",
		"⏰":       "⏰",
		"…":       "",
		"\u200d👍": "",
	} {
		got, ok := ReactionOnly(in)
		if got != want || ok != (want != "") {
			t.Errorf("ReactionOnly(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}
