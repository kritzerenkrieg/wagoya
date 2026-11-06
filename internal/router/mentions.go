package router

import (
	"regexp"
	"sort"
	"strings"
)

// L3: the model tags people by handle ("@P2"); the bot turns that into a
// real WhatsApp mention. A full display name after "@" works too.

var mentionRe = regexp.MustCompile(`@P([0-9]+)\b`)

// Mention is one person tagged in a reply.
type Mention struct {
	Key  string // person key: LID, or "pn:<number>"
	User string // the JID user part shown after "@" in the sent text
}

// MentionUser is the JID user part for a person key.
func MentionUser(key string) string {
	return strings.TrimPrefix(key, "pn:")
}

// ResolveMentions rewrites tags in text. send has "@<user>" for WhatsApp,
// stored has "@Name" for history (handles change between prompts), and
// mentions lists who was tagged, once each. Unknown handles are left as is.
func ResolveMentions(text string, handles, names map[string]string) (send, stored string, mentions []Mention) {
	seen := map[string]bool{}
	add := func(key string) {
		if !seen[key] {
			seen[key] = true
			mentions = append(mentions, Mention{Key: key, User: MentionUser(key)})
		}
	}
	nameOf := func(h string) string {
		if n := strings.TrimSpace(names[h]); n != "" {
			return n
		}
		return "someone"
	}
	send, stored = text, text
	replace := func(s string, user bool) string {
		return mentionRe.ReplaceAllStringFunc(s, func(m string) string {
			h := m[1:]
			key := handles[h]
			if key == "" {
				return m
			}
			add(key)
			if user {
				return "@" + MentionUser(key)
			}
			return "@" + nameOf(h)
		})
	}
	send, stored = replace(send, true), replace(stored, false)
	// "@Full Name", longest names first so "@Ana Maria" beats "@Ana".
	hs := make([]string, 0, len(names))
	for h, n := range names {
		if handles[h] != "" && len([]rune(strings.TrimSpace(n))) >= 3 {
			hs = append(hs, h)
		}
	}
	sort.Slice(hs, func(i, j int) bool { return len(names[hs[i]]) > len(names[hs[j]]) })
	for _, h := range hs {
		tag := "@" + strings.TrimSpace(names[h])
		if i := strings.Index(strings.ToLower(send), strings.ToLower(tag)); i >= 0 {
			add(handles[h])
			send = send[:i] + "@" + MentionUser(handles[h]) + send[i+len(tag):]
		}
	}
	return send, stored, mentions
}
