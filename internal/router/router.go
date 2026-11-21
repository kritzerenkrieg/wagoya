// Package router holds the pure decision logic of the bot: who gets a reply,
// how the trigger is parsed, how the prompt is built and how hidden tags are
// pulled out of Claude's reply. It does no I/O so it can be unit tested.
package router

import (
	"fmt"
	"strings"
	"time"
)

// DenyText is sent to non-whitelisted DMs, without calling Claude (B7, B14).
const DenyText = "ACCESS DENIED // USER IS NOT WHITELISTED"

// MaxPersonaRunes caps a stored persona (D5).
const MaxPersonaRunes = 1500

// Action is what the bot does with an incoming message.
type Action int

const (
	Ignore Action = iota // store nothing, send nothing
	Store                // keep as context only (B2)
	Deny                 // send DenyText (B7)
	Reply                // store and ask Claude (B1, B4)
)

// Incoming is the transport-independent view of a WhatsApp message.
type Incoming struct {
	IsGroup     bool
	FromMe      bool
	Whitelisted bool // sender LID/number is in WHITELIST (B9)
	Addressed   bool // group: the bot is @mentioned or the message replies to it (B1)
	Text        string
}

// Decision is the result of Decide.
type Decision struct {
	Action Action
	Prompt string // text for Claude, trigger stripped (B1)
}

// Decide applies B1, B3, B4 and B7.
func Decide(in Incoming, trigger string) Decision {
	if in.FromMe {
		return Decision{Action: Ignore}
	}
	if in.IsGroup {
		// B1, B3: anyone in the group may trigger by @mentioning the bot or
		// replying to it. B15 keeps the bot out of groups without a
		// whitelisted member.
		if in.Addressed {
			return Decision{Action: Reply, Prompt: strings.TrimSpace(in.Text)}
		}
		return Decision{Action: Store}
	}
	if !in.Whitelisted {
		return Decision{Action: Deny}
	}
	// B4: whitelisted DMs need no prefix, but tolerate the old one (TRIGGER).
	p, ok := ParseTrigger(in.Text, trigger)
	if !ok {
		p = strings.TrimSpace(in.Text)
	}
	return Decision{Action: Reply, Prompt: p}
}

// ParseTrigger reports whether text starts with trigger and returns the rest.
func ParseTrigger(text, trigger string) (string, bool) {
	t := strings.TrimLeft(text, " \t\n")
	if trigger == "" || !strings.HasPrefix(t, trigger) {
		return "", false
	}
	return strings.TrimSpace(t[len(trigger):]), true
}

// Line is one stored chat message used as context.
type Line struct {
	Time        time.Time
	SenderName  string
	SenderID    string // person key: LID, or "pn:<number>" if no LID (D3)
	Whitelisted bool
	FromBot     bool
	Text        string
	Handle      string     // per-request handle like "P2", set by AssignHandles (D4)
	ID          string     // WhatsApp message id, for reactions (L6)
	Reactions   []Reaction // emoji people put on this message (L6)
	ToBot       bool       // K7: addressed to the bot (a DM, a reply to it, or it was tagged)
	To          []string   // K7: other people it is addressed to, by name, when certain
}

// Reaction is one person's emoji reaction to a message (L6).
// Reminder is a scheduled message in this chat (J7), shown with the time
// left until it, worked out by the server (N3).
type Reminder struct {
	At     time.Time
	Text   string
	Repeat string // "" for one-off, else "daily", "hourly", ...
}

// MaxReminders caps the reminders shown in a prompt (N3).
const MaxReminders = 5

type Reaction struct {
	SenderID string // person key
	Name     string
	Emoji    string
}

// Memory is one important fact about a chat kept in SQLite (B5, B12).
type Memory struct {
	Category string
	Text     string
}

// Person is a profile shown to Claude under its per-request handle (D3, D4).
type Person struct {
	Handle      string
	Name        string
	Whitelisted bool
	Facts       []string       // shown inline (CLI mode)
	FactCount   int            // facts stored, found via the search_facts tool (API mode, D7)
	Preferences []string       // standing preferences shown every prompt (K1)
	States      []string       // newest current states, newest first, shown every prompt (K4)
	TaskCounts  map[string]int // the requester's tasks that aren't done, per status (U1, U2)
	TaskCats    []string       // the requester's task categories, most used first (U4)
	HasProfile  bool           // a profile to read with read_profile (V1)
	Profile     string         // the profile itself, shown to the curator (V1)
}

// Prompt is everything rendered into the user message.
type Prompt struct {
	Now       time.Time // current local time, shown as <now>
	Memories  []Memory
	Summary   string     // rolling summary of the chat so far (J8)
	Reminders []Reminder // this chat's upcoming scheduled messages, soonest first (N3)
	People    []Person   // requester first, so it is trimmed last
	History   []Line
	Requester Line
	Text      string
	HasImage  bool
	ImageNote string // which photo is attached: the request's own or the quoted one (F7), or whose recent one (F5)
	Group     bool   // a group chat, shown as <chat> (L3)
	// KeepOwnLines skips Z1's shortening, for providers without chat_history
	// (CLI mode), which couldn't read a shortened line back.
	KeepOwnLines bool
}

// Categories allowed in memory tags (B12).
var Categories = []string{"person", "fact", "decision", "plan", "preference"}

// AssignHandles gives every distinct sender in history, and the requester, a
// handle P1, P2, ... and returns handle -> person key. Claude only ever sees
// handles, so it can't address people who aren't in the prompt (D4).
// P5: numbered by first appearance in history, the requester last if new,
// so the transcript renders the same whoever asks (a stable cache prefix).
func AssignHandles(history []Line, requester *Line) map[string]string {
	byKey := map[string]string{}
	handles := map[string]string{}
	assign := func(l *Line) {
		if l.FromBot || l.SenderID == "" {
			return
		}
		h, ok := byKey[l.SenderID]
		if !ok {
			h = fmt.Sprintf("P%d", len(byKey)+1)
			byKey[l.SenderID] = h
			handles[h] = l.SenderID
		}
		l.Handle = h
	}
	for i := range history {
		assign(&history[i])
	}
	assign(requester)
	return handles
}

// AddHandles gives each key that has no handle yet the next free handle
// (L5: group members who haven't spoken recently).
func AddHandles(handles map[string]string, keys []string) {
	have := map[string]bool{}
	for _, k := range handles {
		have[k] = true
	}
	for _, k := range keys {
		if k == "" || have[k] {
			continue
		}
		have[k] = true
		handles[fmt.Sprintf("P%d", len(handles)+1)] = k
	}
}

var tagSanitizer = strings.NewReplacer("<", "‹", ">", "›")

// clean neutralises tag-like text so chat content can't close or fake the
// prompt's own sections (B9).
func clean(s string) string { return tagSanitizer.Replace(s) }

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
