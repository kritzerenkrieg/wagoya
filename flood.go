package main

// Flood control (M1-M3): checked on the server, before any model call.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
)

const (
	floodLimit   = 10              // M1: messages per person...
	floodWindow  = time.Minute     // ...within this window trigger a check
	floodTimeout = 5 * time.Minute // M1: timeout after a SPAM verdict
	dailyBudget  = 2.0             // M3: USD per local day of model spend
)

const muteUsage = "Usage:\n- /mute @person: mute until /unmute\n- /mute @person 30m: for a while (m, h or d)\n- /unmute @person\n- /mute: list who is muted"

func today() string { return time.Now().Format("2006-01-02") }

// account logs a model request's cost and adds it to today's spend (M3).
func (b *bot) account(what string, st llm.Stats) {
	var day float64
	if st.Cost > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var err error
		if day, err = b.db.AddSpend(ctx, today(), st.Cost); err != nil {
			log.Printf("spend: %v", err)
		}
	}
	logStats(what, st, day)
}

// overBudget reports whether today's spend has reached the cap (M3).
func (b *bot) overBudget(ctx context.Context) bool {
	v, err := b.db.Spend(ctx, today())
	if err != nil {
		log.Printf("spend: %v", err)
		return false
	}
	return v >= dailyBudget
}

// budgetNotice tells a chat, once a day, that only owners get answers (M3).
func (b *bot) budgetNotice(ctx context.Context, chat types.JID, e *events.Message) {
	b.budgetMu.Lock()
	day := today()
	seen := b.budgetNoted[chat.String()] == day
	b.budgetNoted[chat.String()] = day
	b.budgetMu.Unlock()
	log.Printf("over the daily budget: not answering %s in %s", e.Info.Sender, chat)
	if !seen {
		b.send(ctx, chat, "I've used up today's budget, so I'm only answering my owners until midnight.", e)
	}
}

func (b *bot) isMuted(ctx context.Context, key string) bool {
	if key == "" {
		return false
	}
	m, err := b.db.Muted(ctx, key)
	if err != nil {
		log.Printf("muted %s: %v", key, err)
	}
	return m
}

// floodCheck counts a non-owner's message (M1). At floodLimit messages a
// minute, a cheap model call judges them; SPAM, or a failed check, times the
// person out in every chat, with one notice here. It reports a timeout.
func (b *bot) floodCheck(ctx context.Context, e *events.Message, key, text string) bool {
	msgs := b.flood.Note(key, time.Now(), text)
	if msgs == nil {
		return false
	}
	name := e.Info.PushName
	if n, _ := b.db.PersonName(ctx, key); n != "" {
		name = n
	}
	spam, verdict := true, "over budget, not checked"
	if !b.overBudget(ctx) {
		at, texts := make([]time.Time, len(msgs)), make([]string, len(msgs))
		for i, m := range msgs {
			at[i], texts[i] = m.At, m.Text
		}
		var st llm.Stats
		out, err := b.curator.Reply(llm.WithStats(ctx, &st), router.FloodSystem, router.FloodPrompt(name, at, texts), nil, nil)
		b.account("flood check "+e.Info.Chat.String(), st)
		if err != nil {
			verdict = "check failed: " + err.Error()
		} else {
			spam, verdict = router.IsSpam(out), strings.Join(strings.Fields(out), " ")
			if len(verdict) > 40 {
				verdict = verdict[:40]
			}
		}
	}
	log.Printf("flood check %s (%s): %d messages in %s -> %s", key, name, len(msgs), floodWindow, verdict)
	if !spam {
		return false
	}
	if err := b.db.SetMute(ctx, key, name, time.Now().Add(floodTimeout), "flood"); err != nil {
		log.Printf("timeout %s: %v", key, err)
		return false
	}
	log.Printf("timed out %s (%s) for %s for flooding", key, name, floodTimeout)
	b.send(ctx, e.Info.Chat, fmt.Sprintf("%s is timed out for %d minutes for flooding. An owner can lift it with /unmute.", name, int(floodTimeout.Minutes())), nil)
	return true
}

// muteCommand applies /mute or /unmute (M2) and returns the reply.
func (b *bot) muteCommand(ctx context.Context, e *events.Message, cmd router.Command) string {
	if cmd.Name == "mute" && len(cmd.Args) == 0 {
		return b.muteList(ctx)
	}
	var targets []types.JID
	for _, s := range contextInfo(e.Message).GetMentionedJID() {
		if j, err := types.ParseJID(s); err == nil && !b.isMe(j) {
			targets = append(targets, j)
		}
	}
	if len(targets) != 1 {
		return "Tag exactly one person.\n\n" + muteUsage
	}
	lid, pn := b.identify(ctx, targets[0], types.EmptyJID)
	key := personKey(lid, pn)
	if key == "" {
		return "I don't know who that is."
	}
	name, _ := b.db.PersonName(ctx, key)
	if name == "" {
		name = "@" + targets[0].User
	}
	if cmd.Name == "unmute" {
		ok, err := b.db.Unmute(ctx, key)
		switch {
		case err != nil:
			log.Printf("unmute %s: %v", key, err)
			return "Couldn't unmute; nothing changed."
		case !ok:
			return name + " wasn't muted."
		}
		log.Printf("unmuted %s (%s)", key, name)
		return "Unmuted " + name + "."
	}
	if b.allowed(lid, pn) {
		return "Owners can't be muted."
	}
	var until time.Time
	for _, a := range cmd.Args {
		if strings.HasPrefix(a, "@") {
			continue
		}
		d, ok := router.ParseMuteDuration(a)
		if !ok {
			return muteUsage
		}
		until = time.Now().Add(d)
	}
	if err := b.db.SetMute(ctx, key, name, until, "owner"); err != nil {
		log.Printf("mute %s: %v", key, err)
		return "Couldn't mute; nothing changed."
	}
	log.Printf("muted %s (%s) until %v", key, name, until)
	if until.IsZero() {
		return "Muted " + name + " in every chat until /unmute."
	}
	return "Muted " + name + " in every chat until " + clock(until) + "."
}

func (b *bot) muteList(ctx context.Context) string {
	ms, err := b.db.Mutes(ctx)
	if err != nil {
		log.Printf("mutes: %v", err)
		return "Couldn't read the mute list."
	}
	if len(ms) == 0 {
		return "Nobody is muted.\n\n" + muteUsage
	}
	var sb strings.Builder
	sb.WriteString("Muted:")
	for _, m := range ms {
		switch {
		case m.Until.IsZero():
			fmt.Fprintf(&sb, "\n- %s: until /unmute", m.Name)
		case m.Reason == "flood":
			fmt.Fprintf(&sb, "\n- %s: timed out for flooding until %s", m.Name, clock(m.Until))
		default:
			fmt.Fprintf(&sb, "\n- %s: until %s", m.Name, clock(m.Until))
		}
	}
	return sb.String()
}

// clock formats t for a chat reply: the time today, else with the date.
func clock(t time.Time) string {
	if t.Format("2006-01-02") == today() {
		return t.Format("15:04")
	}
	return t.Format("Jan 2 15:04")
}
