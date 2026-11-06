package router

import (
	"strconv"
	"strings"
	"time"
)

// Command is an owner command handled by the bot itself, never by the model
// and never stored (L1, L2, M2).
type Command struct {
	Name string   // "key", "reset", "mute" or "unmute"
	Args []string // the words after the name
}

// ParseCommand recognizes /key, /reset, /mute and /unmute as the first word, after
// any leading @mentions ("@Rin /reset"). ok is false for anything else.
func ParseCommand(text string) (Command, bool) {
	words := strings.Fields(text)
	for len(words) > 0 && strings.HasPrefix(words[0], "@") {
		words = words[1:]
	}
	if len(words) == 0 {
		return Command{}, false
	}
	switch name := strings.ToLower(words[0]); name {
	case "/key", "/reset", "/mute", "/unmute":
		return Command{Name: name[1:], Args: words[1:]}, true
	}
	return Command{}, false
}

// ParseMuteDuration reads a /mute duration such as "30m", "2h" or "1d" (M2).
func ParseMuteDuration(s string) (time.Duration, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) < 2 {
		return 0, false
	}
	n, err := strconv.Atoi(s[:len(s)-1])
	if err != nil || n <= 0 || n > 10000 {
		return 0, false
	}
	switch s[len(s)-1] {
	case 'm':
		return time.Duration(n) * time.Minute, true
	case 'h':
		return time.Duration(n) * time.Hour, true
	case 'd':
		return time.Duration(n) * 24 * time.Hour, true
	}
	return 0, false
}
