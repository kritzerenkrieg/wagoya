package router

import (
	"fmt"
	"regexp"
)

// hedgeRe matches a reply that says it hasn't looked something up instead
// of looking it up (W3, W5).
var hedgeRe = regexp.MustCompile(`(?im)(` +
	`(\bi\s+|^\s*|[.!?…]\s+)(haven['’]?t|have not|didn['’]?t|did not|can['’]?t|cannot|couldn['’]?t|could not|won['’]?t|am unable to) (yet )?(check|verify|confirm|look (it |that )?up|search|find)(ed)?\b` +
	`|\b(would|will|i['’]?d) need to (search|check|look|verify|confirm)` +
	`|\bwithout (checking|searching|looking (it |that )?up|a search|verifying)` +
	`|\bi (don['’]?t|do not) have (the )?(current|up-to-date|recent|live|latest|real-time) (data|figures|numbers|info|information|stats)` +
	`|\bnot (checked|verified|confirmed)` +
	`|\bmy (knowledge|data|info) (may be|might be|is|could be) (out of date|outdated|old|stale)` +
	`|\bso i won['’]?t (rank|judge|say|compare)\b` +
	`)`)

// Hedges reports whether text admits it hasn't checked a fact (W5).
func Hedges(text string) bool { return hedgeRe.MatchString(text) }

// SearchNudge is added to the prompt when a draft hedged without searching
// (W5): the model is told why it is asked again and what to do.
func SearchNudge(draft string) string {
	return fmt.Sprintf("\n\n<retry>Your first draft was %q. It says you haven't checked, and you didn't search. Don't send that. "+
		"Call web_search now (read_page if the snippets lack the facts), then write the reply from what you found.</retry>", clean(draft))
}

// DigNudge is added when a reply searched a little but still says it
// didn't find something (W6).
func DigNudge(draft string) string {
	return fmt.Sprintf("\n\n<retry>Your first draft was %q. It says you didn't find something after only a search or two. Don't send that. "+
		"Search for exactly the missing thing with other words: in the chat's language and the topic's region too, and for local sources "+
		"(shop listings for prices, official pages for rules and schedules). Open the best result with read_page, then write the reply from what you found.</retry>", clean(draft))
}

// MaxDigCalls is the web calls under which a hedging reply is sent back to
// dig further (W6).
const MaxDigCalls = 3

// RetryNudge decides the one retry a reply may get (W5, W6): "" for none.
// webCalls is how many web_search/read_page calls the draft made.
func RetryNudge(draft string, webCalls int) string {
	switch {
	case !Hedges(draft):
		return ""
	case webCalls == 0:
		return SearchNudge(draft)
	case webCalls < MaxDigCalls:
		return DigNudge(draft)
	}
	return ""
}
