package router

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestParseCommand(t *testing.T) {
	for in, want := range map[string]string{
		"/reset":                     "reset|",
		"  /RESET  ":                 "reset|",
		"@Rin /reset":                "reset|",
		"/key":                       "key|",
		"/key tasks.example.com abc": "key|tasks.example.com abc",
		"/key h X-Api-Key: v":        "key|h X-Api-Key: v",
		"/keys":                      "",
		"please /reset":              "",
		"reset":                      "",
		"/resetting the chat":        "",
		"":                           "",
	} {
		c, ok := ParseCommand(in)
		got := ""
		if ok {
			got = c.Name + "|" + strings.Join(c.Args, " ")
		}
		if got != want {
			t.Errorf("ParseCommand(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMuteCommands(t *testing.T) {
	for in, want := range map[string]string{
		"/mute @6281 30m": "mute [@6281 30m]",
		"@Rin /unmute @x": "unmute [@x]",
		"/MUTE":           "mute []",
	} {
		c, ok := ParseCommand(in)
		if got := c.Name + " " + fmt.Sprint(c.Args); !ok || got != want {
			t.Errorf("%q: got %q %v, want %q", in, got, ok, want)
		}
	}
	for in, want := range map[string]time.Duration{"30m": 30 * time.Minute, "2H": 2 * time.Hour, "1d": 24 * time.Hour} {
		if d, ok := ParseMuteDuration(in); !ok || d != want {
			t.Errorf("%q: %v %v", in, d, ok)
		}
	}
	for _, in := range []string{"", "m", "0m", "-1h", "5x", "@x", "1.5h"} {
		if _, ok := ParseMuteDuration(in); ok {
			t.Errorf("%q accepted", in)
		}
	}
	for in, want := range map[string]bool{"SPAM": true, "spam.": true, "OK": false, "ok": false, "**OK**": false, "": true, "maybe": true} {
		if IsSpam(in) != want {
			t.Errorf("IsSpam(%q) != %v", in, want)
		}
	}
}
