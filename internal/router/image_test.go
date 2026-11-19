package router

import (
	"strings"
	"testing"
	"time"
)

// F5: the note says whose photo it is and how old; the request shows it.
func TestImageNote(t *testing.T) {
	now := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		from string
		ago  time.Duration
		want string
	}{
		{"Jane", 10 * time.Second, "the photo Jane posted just now"},
		{"Jane", time.Minute, "the photo Jane posted 1 minute ago"},
		{"", 3 * time.Minute, "the photo someone posted 3 minutes ago"},
	} {
		if got := ImageNote(c.from, now.Add(-c.ago), now); got != c.want {
			t.Errorf("ImageNote(%q, %v) = %q, want %q", c.from, c.ago, got, c.want)
		}
	}
	p := BuildPrompt(Prompt{Requester: Line{SenderName: "Jim"}, Text: "sea", HasImage: true, ImageNote: `the photo <Jane"> posted just now`, Now: now})
	if !strings.Contains(p, `image="attached: the photo ‹Jane\"› posted just now"`) {
		t.Fatalf("request tag: %s", p[strings.Index(p, "<request"):])
	}
}
