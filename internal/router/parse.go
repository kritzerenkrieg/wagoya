package router

import (
	"regexp"
	"slices"
	"strings"
)

// PersonFact is a fact (or a fact to forget) about the person behind Handle.
type PersonFact struct {
	Handle string // "P2" for someone in the prompt, or ""
	Name   string // a person's name when Handle is "" (D4: anyone, any chat)
	Text   string
}

// ParsedReply is Claude's reply split into the visible text and its hidden tags.
type ParsedReply struct {
	Text        string
	Memories    []Memory
	PersonFacts []PersonFact
	Forgets     []PersonFact
	Persona     string
	PersonaSet  bool
}

var (
	memoryTag  = regexp.MustCompile(`(?s)<memory\s+category\s*=\s*"([a-zA-Z]+)"\s*>(.*?)</memory>`)
	personTag  = regexp.MustCompile(`(?s)<person\s+(id|name)\s*=\s*"([^"<>]{1,60})"\s*>(.*?)</person>`)
	forgetTag  = regexp.MustCompile(`(?s)<forget\s+(id|name)\s*=\s*"([^"<>]{1,60})"\s*>(.*?)</forget>`)
	handleRe   = regexp.MustCompile(`^P\d+$`)
	personaTag = regexp.MustCompile(`(?s)<persona>(.*?)</persona>`)
)

// ParseReply removes the hidden tags from Claude's reply (B12, D2, D4).
// Authorisation (whitelist, valid handles) is the caller's job.
func ParseReply(reply string) ParsedReply {
	var r ParsedReply
	out := memoryTag.ReplaceAllStringFunc(reply, func(m string) string {
		sub := memoryTag.FindStringSubmatch(m)
		cat, text := strings.ToLower(sub[1]), oneLine(sub[2])
		if slices.Contains(Categories, cat) && text != "" {
			r.Memories = append(r.Memories, Memory{Category: cat, Text: text})
		}
		return ""
	})
	out = personTag.ReplaceAllStringFunc(out, func(m string) string {
		if f, ok := personFact(personTag.FindStringSubmatch(m)); ok {
			r.PersonFacts = append(r.PersonFacts, f)
		}
		return ""
	})
	out = forgetTag.ReplaceAllStringFunc(out, func(m string) string {
		if f, ok := personFact(forgetTag.FindStringSubmatch(m)); ok {
			r.Forgets = append(r.Forgets, f)
		}
		return ""
	})
	out = personaTag.ReplaceAllStringFunc(out, func(m string) string {
		p := strings.TrimSpace(personaTag.FindStringSubmatch(m)[1])
		if p != "" {
			r.Persona, r.PersonaSet = p, true // D9: over-long ones are recompressed, not cut
		}
		return ""
	})
	r.Text = collapseBlankLines(out)
	return r
}

// personFact reads a <person>/<forget> match: attribute kind, value, text.
func personFact(sub []string) (PersonFact, bool) {
	kind, val, text := sub[1], oneLine(sub[2]), oneLine(sub[3])
	if text == "" || val == "" {
		return PersonFact{}, false
	}
	if kind == "id" {
		if !handleRe.MatchString(val) {
			return PersonFact{}, false
		}
		return PersonFact{Handle: val, Text: text}, true
	}
	return PersonFact{Name: val, Text: text}, true
}

func collapseBlankLines(s string) string {
	lines := strings.Split(s, "\n")
	kept := lines[:0]
	for _, l := range lines {
		if strings.TrimSpace(l) == "" && (len(kept) == 0 || strings.TrimSpace(kept[len(kept)-1]) == "") {
			continue
		}
		kept = append(kept, strings.TrimRight(l, " \t"))
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}
