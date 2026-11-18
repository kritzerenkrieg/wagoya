package router

import (
	"regexp"
	"strings"
)

// K5: every person is their own WhatsApp account. Nothing the bot keeps may
// say two people are the same person, interchangeable, or one's alt, even as
// a joke or a claim someone made: it made Rin answer one person as another.

var identityMerge = regexp.MustCompile(`(?i)\bsame (person|guy|girl|account|human)\b|\binterchangeabl[ey]\b|\bone and the same\b|\balt(ernate)? (account|of)\b|\balter ego\b|\bsock ?puppet\b|\bsecond account\b`)

// MergesIdentities reports whether text says two people are one (K5).
func MergesIdentities(text string) bool { return identityMerge.MatchString(text) }

// DropIdentityMerges removes the sentences and "-" lines of text that say two
// people are one (K5), for the curator's summary.
func DropIdentityMerges(text string) string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if !MergesIdentities(line) {
			out = append(out, line)
			continue
		}
		var keep []string
		for _, s := range regexp.MustCompile(`[^.!?]+[.!?]*\s*`).FindAllString(line, -1) {
			if !MergesIdentities(s) {
				keep = append(keep, s)
			}
		}
		if l := strings.TrimSpace(strings.Join(keep, "")); l != "" && l != "-" {
			out = append(out, l)
		}
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
