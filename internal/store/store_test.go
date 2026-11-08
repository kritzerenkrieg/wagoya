package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/router"
)

func TestHistoryWindowPersists(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bot.db")
	s, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 40; i++ {
		if err := s.AddMessage(ctx, "g1", router.Line{Time: time.Unix(int64(i), 0), SenderName: "a", Text: fmt.Sprint(i)}); err != nil {
			t.Fatal(err)
		}
	}
	s.AddMessage(ctx, "g2", router.Line{Time: time.Now(), Text: "other"})
	s.Close()

	s, err = Open(path, 32) // restart
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := s.History(ctx, "g1")
	if err != nil {
		t.Fatal(err)
	}
	if len(h) != 32 || h[0].Text != "8" || h[31].Text != "39" {
		t.Fatalf("window wrong: len=%d first=%q last=%q", len(h), h[0].Text, h[len(h)-1].Text)
	}
}

func TestMemoriesPerChatSearch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.AddMemories(ctx, "g1", []router.Memory{
		{Category: "plan", Text: "Dinner is on Friday at 7pm."},
		{Category: "person", Text: "Budi is the treasurer."},
		{Category: "plan", Text: "Dinner is on Friday at 7pm."}, // duplicate
	})
	s.AddMemories(ctx, "g2", []router.Memory{{Category: "fact", Text: "Dinner budget is secret."}})

	got, err := s.SearchMemories(ctx, "g1", `when is "dinner"? OR NEAR(x)`, 8)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "Dinner is on Friday at 7pm." {
		t.Fatalf("got %+v", got)
	}
	if got, _ := s.SearchMemories(ctx, "g1", "!!", 8); len(got) != 0 {
		t.Fatalf("empty query returned %+v", got)
	}
}

func TestPersonaHistory(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bot.db")
	s, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	if p, _ := s.Persona(ctx); p != "" {
		t.Fatalf("fresh persona = %q", p)
	}
	s.SetPersona(ctx, "You are Rin.", "lidA")
	s.SetPersona(ctx, "You are Rin, sarcastic.", "lidA")
	s.Close()

	s, err = Open(path, 32) // restart
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if p, _ := s.Persona(ctx); p != "You are Rin, sarcastic." {
		t.Fatalf("persona = %q", p)
	}
	if h, _ := s.PersonaHistory(ctx, 10); len(h) != 2 || h[1] != "You are Rin." {
		t.Fatalf("history = %v", h)
	}
}

func TestPeopleGlobalProfiles(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	if err := s.UpsertPerson(ctx, "lidA", "lidA", "", "Budi", now); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertPerson(ctx, "lidA", "", "6281", "", now); err != nil { // name kept, pn filled
		t.Fatal(err)
	}
	var name, pn string
	s.db.QueryRow(`SELECT name, pn FROM people WHERE key='lidA'`).Scan(&name, &pn)
	if name != "Budi" || pn != "6281" {
		t.Fatalf("upsert merged badly: name=%q pn=%q", name, pn)
	}

	s.UpsertPerson(ctx, "lidB", "lidB", "", "Ani", now)
	s.AddPersonFacts(ctx, "lidA", Source{Chat: "group1", Tier: TierSelf}, []string{"Budi is the treasurer.", "Budi is the treasurer."})
	s.AddPersonFacts(ctx, "lidA", Source{Chat: "group2", Tier: TierSelf}, []string{"Budi likes durian."}) // learned in another chat
	for i := range 12 {
		s.AddPersonFacts(ctx, "lidB", Source{Chat: "dm", Tier: TierSelf}, []string{fmt.Sprintf("fact %02d", i)})
	}

	got, err := s.PersonFacts(ctx, []string{"lidA", "lidB", "nobody"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got["lidA"]) != 2 || got["lidA"][1] != "Budi likes durian." {
		t.Fatalf("lidA facts (global across chats): %v", got["lidA"])
	}
	if len(got["lidB"]) != 12 || got["lidB"][0] != "fact 00" || got["lidB"][11] != "fact 11" {
		t.Fatalf("lidB facts must be uncapped: %v", got["lidB"])
	}
	if len(got["nobody"]) != 0 {
		t.Fatal("unknown person has facts")
	}

	if ok, _ := s.InvalidateFact(ctx, "lidA", "  budi likes DURIAN. ", TierOwner, "test"); !ok {
		t.Fatal("forget did not match")
	}
	if ok, _ := s.InvalidateFact(ctx, "lidB", "Budi is the treasurer.", TierOwner, "test"); ok {
		t.Fatal("forget crossed people")
	}
	got, _ = s.PersonFacts(ctx, []string{"lidA"})
	if len(got["lidA"]) != 1 {
		t.Fatalf("after forget: %v", got["lidA"])
	}
}

func TestSearchPersonFacts(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "bot.db")
	s, err := Open(path, 32)
	if err != nil {
		t.Fatal(err)
	}
	s.AddPersonFacts(ctx, "john", Source{Chat: "dm", Tier: TierSelf}, []string{"Job: Nurse at Acme Corp.", "Origin: Lisbon, Portugal."})
	s.AddPersonFacts(ctx, "budi", Source{Chat: "g1", Tier: TierSelf}, []string{"Job: treasurer of the group."})
	s.Close()

	s, err = Open(path, 32) // restart: index rebuilt and still consistent
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	got, err := s.SearchPersonFacts(ctx, []string{"john", "budi"}, "where is his job?", 30, false)
	if err != nil || len(got) != 2 {
		t.Fatalf("job search: %+v %v", got, err)
	}
	if got, _ := s.SearchPersonFacts(ctx, []string{"budi"}, "lisbon", 30, false); len(got) != 0 {
		t.Fatalf("searched a person not in scope: %+v", got)
	}
	if got, _ := s.SearchPersonFacts(ctx, []string{"john"}, "nurses", 30, false); len(got) != 1 || got[0].Person != "john" {
		t.Fatalf("stemmed search: %+v", got)
	}
	if got, _ := s.SearchPersonFacts(ctx, []string{"john"}, "", 30, false); len(got) != 2 || got[0].Text != "Origin: Lisbon, Portugal." {
		t.Fatalf("list all (newest first): %+v", got)
	}

	// Forget keeps the index in sync.
	s.InvalidateFact(ctx, "john", "Origin: Lisbon, Portugal.", TierOwner, "test")
	if got, _ := s.SearchPersonFacts(ctx, []string{"john"}, "lisbon", 30, false); len(got) != 0 {
		t.Fatalf("forgotten fact still found: %+v", got)
	}
	if n, _ := s.FactCounts(ctx, []string{"john", "budi", "x"}); n["john"] != 1 || n["budi"] != 1 || n["x"] != 0 {
		t.Fatalf("counts: %v", n)
	}
}

func TestPersonName(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.UpsertPerson(ctx, "lidF", "lidF", "", "Dewi Lestari", time.Now())
	if n, _ := s.PersonName(ctx, "lidF"); n != "Dewi Lestari" {
		t.Fatalf("name = %q", n)
	}
	if n, err := s.PersonName(ctx, "nobody"); n != "" || err != nil {
		t.Fatalf("unknown = %q %v", n, err)
	}
}

func TestGlobalPeopleSearch(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.UpsertPerson(ctx, "john", "john", "", "Alex Rivera", now)
	s.UpsertPerson(ctx, "omar", "omar", "", "Omar", now)
	s.UpsertPerson(ctx, "kris", "kris", "", "Kris", now)
	s.AddPersonFacts(ctx, "john", Source{Chat: "dm", Tier: TierSelf}, []string{"Origin: Lisbon, Portugal."})
	s.AddPersonFacts(ctx, "omar", Source{Chat: "group", Tier: TierSelf}, []string{"Relation: John's friend.", "Knows: humanitarian law."})

	// By name, also from a longer display name; short words don't match.
	if p, _ := s.FindPeople(ctx, "Bapak Omar", 5); len(p) != 1 || p[0].Key != "omar" {
		t.Fatalf("display name lookup: %+v", p)
	}
	if p, _ := s.FindPeople(ctx, "alex", 5); len(p) != 1 || p[0].Key != "john" {
		t.Fatalf("partial name lookup: %+v", p)
	}
	if p, _ := s.FindPeople(ctx, "is", 5); len(p) != 0 {
		t.Fatalf("2-letter query matched: %+v", p)
	}

	// Global search: a name in the query pulls that person's facts, from any chat.
	got, err := s.SearchAllFacts(ctx, "who is omar?", 30, false)
	if err != nil || len(got) != 2 || got[0].Person != "omar" {
		t.Fatalf("name search: %+v %v", got, err)
	}
	if got, _ := s.SearchAllFacts(ctx, "humanitarian", 30, false); len(got) != 1 || got[0].Person != "omar" {
		t.Fatalf("fact search: %+v", got)
	}
	if got, _ := s.SearchAllFacts(ctx, "", 30, false); len(got) != 3 {
		t.Fatalf("list all: %+v", got)
	}

	// Saving by name: a known person resolves, an unknown one gets a profile.
	if p, _ := s.PersonByName(ctx, "omar", now); p.Key != "omar" {
		t.Fatalf("resolve known: %+v", p)
	}
	p, err := s.PersonByName(ctx, "Dewi  Lestari", now)
	if err != nil || p.Key != "name:dewi lestari" || p.Name != "Dewi Lestari" {
		t.Fatalf("create unknown: %+v %v", p, err)
	}
	if p2, _ := s.PersonByName(ctx, "dewi", now); p2.Key != p.Key {
		t.Fatalf("created profile not found again: %+v", p2)
	}
}

func TestSameNameTags(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.UpsertPerson(ctx, "900000000009249", "900000000009249", "", "Omar", now)
	s.UpsertPerson(ctx, "111111111111180", "111111111111180", "", "omar", now.Add(-time.Hour))
	s.UpsertPerson(ctx, "222", "222", "", "Ani", now)
	if IDTag("900000000009249") != "9249" || IDTag("name:dewi") != "name" || IDTag("pn:62812") != "2812" {
		t.Fatal("IDTag")
	}
	if taken, _ := s.NameTaken(ctx, "Omar", "900000000009249"); !taken {
		t.Fatal("shared name not detected")
	}
	if taken, _ := s.NameTaken(ctx, "Ani", "222"); taken {
		t.Fatal("unique name flagged")
	}
	if p, _ := s.FindPeople(ctx, "Omar", 5); len(p) != 2 {
		t.Fatalf("both Omars: %+v", p)
	}
	if p, _ := s.FindPeople(ctx, "Omar #1180", 5); len(p) != 1 || p[0].Key != "111111111111180" {
		t.Fatalf("tag pick: %+v", p)
	}
	if p, _ := s.PersonByName(ctx, "omar #1180", now); p.Key != "111111111111180" {
		t.Fatalf("save by tag: %+v", p)
	}
	if p, _ := s.PersonByName(ctx, "Budi #7777", now); p.Key != "name:budi" || p.Name != "Budi" {
		t.Fatalf("unknown tag: %+v", p)
	}
}

func TestCuratorState(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	add := func(n int) {
		for i := range n {
			s.AddMessage(ctx, "g", router.Line{Time: time.Now(), SenderID: "a", SenderName: "A", Text: fmt.Sprintf("m%d", i)})
		}
	}
	add(15)
	if got, _ := s.TakeUnread(ctx, "g", 16); got != nil {
		t.Fatalf("took %d before reaching 16", len(got))
	}
	add(1)
	got, err := s.TakeUnread(ctx, "g", 16)
	if err != nil || len(got) != 16 || got[0].Text != "m0" || got[15].Text != "m0" {
		t.Fatalf("take 16: %d %v", len(got), err)
	}
	if n, _ := s.UnreadCount(ctx, "g"); n != 0 {
		t.Fatalf("unread after take: %d", n)
	}
	add(3)
	if got, _ := s.TakeUnread(ctx, "g", 1); len(got) != 3 { // idle pass takes any
		t.Fatalf("idle take: %d", len(got))
	}
	if got, _ := s.TakeUnread(ctx, "other", 1); got != nil {
		t.Fatal("empty chat returned lines")
	}
}

func TestAliasAndMerge(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	s.UpsertPerson(ctx, "lidN", "lidN", "", "Yan", now)
	s.AddPersonFacts(ctx, "lidN", Source{Chat: "g", Tier: TierSelf}, []string{"Alias: Wayan"})
	if p, _ := s.FindPeople(ctx, "wayan", 5); len(p) != 1 || p[0].Key != "lidN" {
		t.Fatalf("alias lookup: %+v", p)
	}
	// A name-only profile created earlier merges into the real one.
	s.UpsertPerson(ctx, "lidD", "lidD", "", "dewi_real", now)
	ghost, _ := s.PersonByName(ctx, "Dewi Lestari", now)
	s.AddPersonFacts(ctx, ghost.Key, Source{Chat: "g", Tier: TierSelf}, []string{"Habit: posts war photos."})
	if ok, err := s.MergePerson(ctx, ghost.Key, "lidD"); !ok || err != nil {
		t.Fatalf("merge: %v %v", ok, err)
	}
	if f, _ := s.PersonFacts(ctx, []string{"lidD", ghost.Key}); len(f["lidD"]) != 1 || len(f[ghost.Key]) != 0 {
		t.Fatalf("facts after merge: %+v", f)
	}
	if ok, _ := s.MergePerson(ctx, "lidN", "lidD"); ok {
		t.Fatal("merged away a real profile")
	}
}

func TestSummary(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if got, err := s.Summary(ctx, "g"); got != "" || err != nil {
		t.Fatalf("empty: %q %v", got, err)
	}
	s.SetSummary(ctx, "g", "one")
	s.SetSummary(ctx, "g", "two")
	if got, _ := s.Summary(ctx, "g"); got != "two" {
		t.Fatalf("got %q", got)
	}
	if got, _ := s.Summary(ctx, "other"); got != "" {
		t.Fatal("summary leaked to another chat")
	}
}

// J7: scheduled jobs survive a restart.
func TestJobsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "j.db")
	s, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(time.Minute).Truncate(time.Second)
	id, err := s.AddJob(context.Background(), Job{Chat: "g", CreatedBy: "john", CreatedName: "John", Text: "eat", NextAt: at, Repeat: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path, 16); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if due, _ := s.DueJobs(context.Background(), at.Add(-time.Second)); len(due) != 0 {
		t.Fatal("not yet due")
	}
	due, _ := s.DueJobs(context.Background(), at)
	if len(due) != 1 || due[0].ID != id || due[0].Text != "eat" || due[0].Repeat != "daily" || !due[0].NextAt.Equal(at) {
		t.Fatalf("after restart: %+v", due)
	}
}

// L2: /reset drops the transcript and summary of one chat only; memories,
// facts and jobs stay, and the curator only sees messages after the reset.
func TestResetChat(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 32)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	for _, c := range []string{"a", "b"} {
		s.AddMessage(ctx, c, router.Line{Time: now, SenderID: "john", SenderName: "John", Text: "hi " + c})
		s.SetSummary(ctx, c, "summary "+c)
	}
	s.AddMemories(ctx, "a", []router.Memory{{Category: "note", Text: "keeps this"}})
	s.AddPersonFacts(ctx, "john", Source{Chat: "a", Tier: TierSelf}, []string{"Likes: tea"})
	s.TakeUnread(ctx, "a", 1)
	n, err := s.ResetChat(ctx, "a")
	if err != nil || n != 1 {
		t.Fatalf("reset: %d %v", n, err)
	}
	if h, _ := s.History(ctx, "a"); len(h) != 0 {
		t.Fatalf("history after reset: %v", h)
	}
	if sum, _ := s.Summary(ctx, "a"); sum != "" {
		t.Fatalf("summary after reset: %q", sum)
	}
	if h, _ := s.History(ctx, "b"); len(h) != 1 {
		t.Fatal("other chat touched")
	}
	if m, _ := s.SearchMemories(ctx, "a", "keeps", 5); len(m) != 1 {
		t.Fatal("memory lost")
	}
	if c, _ := s.FactCounts(ctx, []string{"john"}); c["john"] != 1 {
		t.Fatal("fact lost")
	}
	s.AddMessage(ctx, "a", router.Line{Time: now, SenderID: "john", SenderName: "John", Text: "fresh"})
	if got, _ := s.TakeUnread(ctx, "a", 1); len(got) != 1 || got[0].Text != "fresh" {
		t.Fatalf("curator after reset: %v", got)
	}
}

// L6: reactions attach to their message by WhatsApp id, change, go away when
// removed, ignore unknown messages, and are pruned with their message.
func TestReactions(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	s.AddMessage(ctx, "g", router.Line{Time: now, SenderName: "Rin", FromBot: true, Text: "futsal at 20:00?", ID: "m1"})
	set := func(id, who, emoji string) bool {
		ok, err := s.SetReaction(ctx, "g", id, who, strings.ToUpper(who[:1])+who[1:], emoji, now)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !set("m1", "john", "👍") || !set("m1", "ana", "😂") || !set("m1", "john", "❤️") {
		t.Fatal("reaction on a stored message not recorded")
	}
	if set("nope", "john", "👍") {
		t.Fatal("reaction on an unknown message recorded")
	}
	h, _ := s.History(ctx, "g")
	byWho := map[string]string{}
	for _, r := range h[0].Reactions {
		byWho[r.SenderID] = r.Emoji
	}
	if len(h) != 1 || len(h[0].Reactions) != 2 || byWho["john"] != "❤️" || byWho["ana"] != "😂" {
		t.Fatalf("history: %+v", h)
	}
	set("m1", "ana", "")
	if h, _ := s.History(ctx, "g"); len(h[0].Reactions) != 1 || h[0].Reactions[0].Name != "John" {
		t.Fatalf("after removal: %+v", h[0].Reactions)
	}
	s.SetClock(func() time.Time { return now.Add(MessageTTL + time.Hour) }) // L9: past the retention
	// P6: m1 stays while it is in the window's block, then goes with it.
	s.AddMessage(ctx, "g", router.Line{Time: now, SenderName: "John", SenderID: "john", Text: "a", ID: "m2"})
	if h, _ := s.History(ctx, "g"); len(h) != 2 || h[0].ID != "m1" {
		t.Fatalf("m1 left the window early: %+v", h)
	}
	for i := 0; i < WindowBlock; i++ {
		s.AddMessage(ctx, "g", router.Line{Time: now, SenderName: "John", SenderID: "john", Text: "b", ID: fmt.Sprintf("x%d", i)})
	}
	s.History(ctx, "g")                                                                                     // the window moves past m1
	s.AddMessage(ctx, "g", router.Line{Time: now, SenderName: "John", SenderID: "john", Text: "c", ID: "m3"}) // m1 pruned
	var n int
	s.db.QueryRow(`SELECT count(*) FROM reactions`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d reactions left after their message was pruned", n)
	}
}

func TestOlderMessages(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 2)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	s.SetClock(func() time.Time { return now })
	add := func(chat string, age time.Duration, who, text string) {
		if err := s.AddMessage(ctx, chat, router.Line{Time: now.Add(-age), SenderName: who, SenderID: strings.ToLower(who), Text: text}); err != nil {
			t.Fatal(err)
		}
	}
	add("g", 80*time.Hour, "John", "too old: futsal booking")
	add("g", 10*time.Hour, "Omar", "Messi is overrated")
	add("g", 9*time.Hour, "John", "Messi has 8 Ballon d'Ors")
	add("g", 8*time.Hour, "Budi", "lunch?")
	add("other", time.Hour, "Ana", "Messi secret in another chat")
	add("g", 2*time.Hour, "John", "window one")
	add("g", time.Hour, "Omar", "window two")

	text := func(ls []router.Line) string {
		var out []string
		for _, l := range ls {
			out = append(out, l.Text)
		}
		return strings.Join(out, " | ")
	}
	got, err := s.OlderMessages(ctx, "g", "", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if want := "Messi is overrated | Messi has 8 Ballon d'Ors | lunch?"; text(got) != want {
		t.Fatalf("page back: got %q, want %q (80h-old message dropped, window and other chat excluded)", text(got), want)
	}
	if got, _ := s.OlderMessages(ctx, "g", "who said Messi ballon", 1, 0); text(got) != "Messi has 8 Ballon d'Ors" {
		t.Fatalf("best match first: %q", text(got))
	}
	if got, _ := s.OlderMessages(ctx, "g", "omar", 10, 0); text(got) != "Messi is overrated" {
		t.Fatalf("by sender name: %q", text(got))
	}
	if got, _ := s.OlderMessages(ctx, "g", "", 10, 3); len(got) != 0 {
		t.Fatalf("offset past the end: %q", text(got))
	}
	if h, _ := s.History(ctx, "g"); text(h) != "window one | window two" {
		t.Fatalf("window: %q", text(h))
	}
}

func TestMutesAndSpend(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Unix(1_800_000_000, 0)
	s.SetClock(func() time.Time { return now })
	muted := func(k string) bool {
		m, err := s.Muted(ctx, k)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	if muted("a") {
		t.Fatal("muted by default")
	}
	s.SetMute(ctx, "a", "Andi", time.Time{}, "owner")
	s.SetMute(ctx, "b", "Budi", now.Add(5*time.Minute), "flood")
	s.SetMute(ctx, "c", "Cici", now.Add(-time.Minute), "flood") // already over
	if !muted("a") || !muted("b") || muted("c") {
		t.Fatal("mute state wrong")
	}
	ms, _ := s.Mutes(ctx)
	if len(ms) != 2 || ms[0].Name != "Budi" || ms[1].Name != "Andi" || !ms[1].Until.IsZero() {
		t.Fatalf("list: %+v", ms)
	}
	now = now.Add(6 * time.Minute)
	if muted("b") || !muted("a") {
		t.Fatal("timeout didn't end, or forever mute did")
	}
	if ok, _ := s.Unmute(ctx, "a"); !ok || muted("a") {
		t.Fatal("unmute")
	}
	if ok, _ := s.Unmute(ctx, "c"); ok {
		t.Fatal("unmute of an ended timeout reported active")
	}

	if v, _ := s.Spend(ctx, "2026-11-06"); v != 0 {
		t.Fatal("spend not zero")
	}
	s.AddSpend(ctx, "2026-11-06", 0.25)
	if v, _ := s.AddSpend(ctx, "2026-11-06", 0.5); v != 0.75 {
		t.Fatalf("total %v", v)
	}
	if v, _ := s.Spend(ctx, "2026-11-07"); v != 0 {
		t.Fatal("days not separate")
	}
}

// K5: no path stores a fact or memory saying two people are one.
func TestIdentityMergesNotStored(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	s.UpsertPerson(ctx, "john", "john", "", "John", time.Now())
	s.AddPersonFacts(ctx, "john", Source{Chat: "g", Tier: TierInferred}, []string{"Claim: John and Budi are the same person", "Likes: chess"})
	s.AddMemories(ctx, "g", []router.Memory{{Category: "fact", Text: "John and Budi are interchangeable"}, {Category: "plan", Text: "Futsal Friday"}})
	facts, _ := s.PersonFacts(ctx, []string{"john"})
	if len(facts["john"]) != 1 || !strings.Contains(facts["john"][0], "chess") {
		t.Fatalf("facts: %v", facts["john"])
	}
	if m, _ := s.SearchMemories(ctx, "g", "John Budi interchangeable futsal", 10); len(m) != 1 || m[0].Text != "Futsal Friday" {
		t.Fatalf("memories: %+v", m)
	}
}

func TestRewriteSummaries(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	s.SetSummary(ctx, "a", "John (P1) and Budi.")
	s.SetSummary(ctx, "b", "Nothing to fix.")
	n, err := s.RewriteSummaries(ctx, func(t string) string { return strings.ReplaceAll(t, " (P1)", "") })
	if got, _ := s.Summary(ctx, "a"); err != nil || n != 1 || got != "John and Budi." {
		t.Fatalf("rewrite = %d %v %q", n, err, got)
	}
}

func TestWindowBlocks(t *testing.T) {
	// P6: the window shows limit..limit+WindowBlock lines and its first line
	// stays put until it drops a whole block.
	const limit = 4
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), limit)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	add := func(i int) {
		s.AddMessage(ctx, "g", router.Line{Time: time.Now(), SenderName: "John", SenderID: "john", Text: fmt.Sprint(i)})
	}
	first := func() (string, int) {
		h, _ := s.History(ctx, "g")
		return h[0].Text, len(h)
	}
	for i := 1; i <= limit; i++ {
		add(i)
	}
	if f, n := first(); f != "1" || n != limit {
		t.Fatalf("start: first %s, %d lines", f, n)
	}
	for i := limit + 1; i <= limit+WindowBlock; i++ {
		add(i)
		if f, n := first(); f != "1" || n != i {
			t.Fatalf("after %d: first %s, %d lines; want 1 and %d", i, f, n, i)
		}
	}
	add(limit + WindowBlock + 1) // one more: drop the block, show exactly limit
	if f, n := first(); f != fmt.Sprint(WindowBlock+2) || n != limit {
		t.Fatalf("after the block: first %s, %d lines", f, n)
	}
	add(limit + WindowBlock + 2)
	if f, n := first(); f != fmt.Sprint(WindowBlock+2) || n != limit+1 {
		t.Fatalf("next block: first %s, %d lines", f, n)
	}
}
