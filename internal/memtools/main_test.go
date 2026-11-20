package memtools

import (
	"os"
	"testing"
	"time"
)

// TestMain runs the tests in the fixtures' zone, as the bot runs in its
// !time zone (L15): RemindAt and stored times use time.Local, not the
// machine's zone.
func TestMain(m *testing.M) {
	time.Local = wib
	os.Exit(m.Run())
}
