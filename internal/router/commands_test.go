package router

import (
	"strings"
	"testing"
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
