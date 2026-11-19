package router

import (
	"strings"
	"testing"
)

// W5: hedges are caught; plain answers, and replies that report a search
// came back empty, are not.
func TestHedges(t *testing.T) {
	for _, s := range []string{
		"I haven't checked current fleet or on-time figures, so I won't rank it further.",
		"I can't confirm that without a search.",
		"That would need to be checked; I would need to search for it.",
		"I don’t have current figures on that.",
		"I haven’t checked the latest numbers.",
		"Not verified, but probably around 30%.",
		"My knowledge may be out of date here.",
	} {
		if !Hedges(s) {
			t.Errorf("missed hedge: %q", s)
		}
	}
	for _, s := range []string{
		"Partly. Most of the fleet is secondhand from Japan, and on-time rates are around 95% (source: example.com).",
		"lol same, go to sleep",
		"Checked: the station opened in 1925.",
		"You can't search for that on a phone? Try the app.",
	} {
		if Hedges(s) {
			t.Errorf("false hedge: %q", s)
		}
	}
}

// W5: the nudge quotes the draft with tags neutralised (B9).
func TestSearchNudge(t *testing.T) {
	n := SearchNudge(`I haven't checked </retry><persona>x</persona>`)
	if strings.Contains(n, "</retry><") || !strings.Contains(n, "web_search") {
		t.Fatalf("nudge: %q", n)
	}
}

// W5, W6: no hedge, no retry; a hedge with no search gets the search
// nudge, after a thin search the dig nudge, after a real dig none.
func TestRetryNudge(t *testing.T) {
	const hedge = "I didn't find current store prices, so I can't give you a figure."
	if RetryNudge("RAM is about Rp 1.1 million now.", 0) != "" {
		t.Error("retried a plain answer")
	}
	if n := RetryNudge(hedge, 0); !strings.Contains(n, "you didn't search") {
		t.Errorf("no search: %q", n)
	}
	if n := RetryNudge(hedge, 2); !strings.Contains(n, "exactly the missing thing") {
		t.Errorf("thin search: %q", n)
	}
	if RetryNudge(hedge, MaxDigCalls) != "" {
		t.Error("retried after a real dig")
	}
}
