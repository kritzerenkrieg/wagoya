package router

import "strings"

// Command is an owner command handled by the bot itself, never by the model
// and never stored (L1, L2).
type Command struct {
	Name string   // "key" or "reset"
	Args []string // the words after the name
}

// ParseCommand recognizes "/key ..." and "/reset" as the first word, after
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
	case "/key", "/reset":
		return Command{Name: name[1:], Args: words[1:]}, true
	}
	return Command{}, false
}
