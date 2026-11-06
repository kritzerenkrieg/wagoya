package router

import (
	"regexp"
	"sort"
	"strings"
)

// L3: the model tags people by handle ("@P2"); the bot turns that into a
// real WhatsApp mention. A full display name after "@" works too.
// L10: handles change between prompts, so text kept for later (a scheduled
// message) is pinned first: each tag becomes "@{key}", a person key that
// stays valid, and is expanded into a mention only when it is sent.

var (
	mentionRe = regexp.MustCompile(`@P([0-9]+)\b`)
	pinnedRe  = regexp.MustCompile(`@\{([^{}\s]+)\}`)
)

// Mention is one person tagged in a reply.
type Mention struct {
	Key  string // person key: LID, or "pn:<number>"
	User string // the JID user part shown after "@" in the sent text
}

// MentionUser is the JID user part for a person key.
func MentionUser(key string) string {
	return strings.TrimPrefix(key, "pn:")
}

// PinMentions rewrites tags in text ("@P2", or "@Full Name" of someone with a
// handle) as "@{key}". Unknown handles are left as is. Any "@{" already in
// the text is broken up first, so text can't tag someone by key itself.
func PinMentions(text string, handles, names map[string]string) string {
	text = strings.ReplaceAll(text, "@{", "@ {")
	text = mentionRe.ReplaceAllStringFunc(text, func(m string) string {
		if key := handles[m[1:]]; key != "" {
			return "@{" + key + "}"
		}
		return m
	})
	// "@Full Name", longest names first so "@Ana Maria" beats "@Ana".
	hs := make([]string, 0, len(names))
	for h, n := range names {
		if handles[h] != "" && len([]rune(strings.TrimSpace(n))) >= 3 {
			hs = append(hs, h)
		}
	}
	sort.Slice(hs, func(i, j int) bool { return len(names[hs[i]]) > len(names[hs[j]]) })
	for _, h := range hs {
		tag := strings.ToLower("@" + strings.TrimSpace(names[h]))
		for {
			i := strings.Index(strings.ToLower(text), tag)
			if i < 0 {
				break
			}
			text = text[:i] + "@{" + handles[h] + "}" + text[i+len(tag):]
		}
	}
	return text
}

// ExpandMentions turns pinned "@{key}" tags into what WhatsApp needs: send
// has "@<user>", stored has "@Name" for history (nameOf may return "" for
// "someone"), and mentions lists who was tagged, once each.
func ExpandMentions(text string, nameOf func(key string) string) (send, stored string, mentions []Mention) {
	seen := map[string]bool{}
	send = pinnedRe.ReplaceAllStringFunc(text, func(m string) string {
		key := m[2 : len(m)-1]
		if !seen[key] {
			seen[key] = true
			mentions = append(mentions, Mention{Key: key, User: MentionUser(key)})
		}
		return "@" + MentionUser(key)
	})
	stored = pinnedRe.ReplaceAllStringFunc(text, func(m string) string {
		if n := strings.TrimSpace(nameOf(m[2 : len(m)-1])); n != "" {
			return "@" + n
		}
		return "@someone"
	})
	return send, stored, mentions
}

// ResolveMentions is PinMentions then ExpandMentions, for a reply sent now.
func ResolveMentions(text string, handles, names map[string]string) (send, stored string, mentions []Mention) {
	byKey := map[string]string{}
	for h, k := range handles {
		byKey[k] = names[h]
	}
	return ExpandMentions(PinMentions(text, handles, names), func(k string) string { return byKey[k] })
}
