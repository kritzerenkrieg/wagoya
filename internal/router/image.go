package router

import (
	"fmt"
	"time"
)

// ImageNote says whose recent photo is attached to a tag that quoted
// nothing (F5), e.g. "the photo Jane posted 2 minutes ago".
func ImageNote(from string, at, now time.Time) string {
	if from == "" {
		from = "someone"
	}
	ago := "just now"
	if m := int(now.Sub(at).Round(time.Minute) / time.Minute); m == 1 {
		ago = "1 minute ago"
	} else if m > 1 {
		ago = fmt.Sprintf("%d minutes ago", m)
	}
	return fmt.Sprintf("the photo %s posted %s", from, ago)
}
