package store

import (
	"context"
	"fmt"
	"path/filepath"
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
