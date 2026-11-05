package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func openTest(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "s.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }
	return s, &now
}

func texts(fs []Fact) []string {
	var out []string
	for _, f := range fs {
		out = append(out, f.String())
	}
	return out
}

// J1: old files get the new columns; facts become legacy, keep their time,
// and labels map to kinds. Nothing is deleted.
func TestMigrateFacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE people (key TEXT PRIMARY KEY, lid TEXT NOT NULL DEFAULT '', pn TEXT NOT NULL DEFAULT '',
		name TEXT NOT NULL DEFAULT '', first_seen INTEGER NOT NULL, last_seen INTEGER NOT NULL);
	CREATE TABLE person_facts (id INTEGER PRIMARY KEY AUTOINCREMENT, person TEXT NOT NULL REFERENCES people(key),
		text TEXT NOT NULL, chat TEXT NOT NULL, ts INTEGER NOT NULL, UNIQUE(person, text));
	INSERT INTO people VALUES ('a', 'a', '', 'Ana', 1, 1);
	INSERT INTO person_facts(person, text, chat, ts) VALUES
		('a', 'Job: nurse', 'g', 1000), ('a', 'Claim: met the president', 'g', 1001),
		('a', 'Joke: always late', 'g', 1002), ('a', 'Behavior: posts war photos', 'g', 1003);`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.SearchPersonFacts(context.Background(), []string{"a"}, "", 30, false)
	if err != nil || len(got) != 4 {
		t.Fatalf("facts after migration: %v %v", got, err)
	}
	kinds := map[string]Kind{}
	for _, f := range got {
		if f.Tier != TierLegacy || f.ValidFrom.Unix() < 1000 || !f.Valid() {
			t.Fatalf("bad migrated fact %+v", f)
		}
		kinds[f.Text] = f.Kind
	}
	if kinds["Job: nurse"] != KindFact || kinds["Claim: met the president"] != KindClaim ||
		kinds["Joke: always late"] != KindJoke || kinds["Behavior: posts war photos"] != KindClaim {
		t.Fatalf("kinds: %v", kinds)
	}
	// Opening again is a no-op.
	s.Close()
	if s, err = Open(path, 16); err != nil {
		t.Fatal(err)
	}
}

// J1: invalidated facts leave search but stay as history; saying them again
// revives them.
func TestInvalidateKeepsHistory(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	s.AddPersonFacts(ctx, "ken", Source{Chat: "dm", SaidBy: "ken", Tier: TierSelf}, []string{"Likes: durian"})
	*now = now.Add(time.Hour)
	if ok, err := s.InvalidateFact(ctx, "ken", "likes: DURIAN ", TierSelf, "said he hates it now"); !ok || err != nil {
		t.Fatalf("invalidate: %v %v", ok, err)
	}
	if got, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "durian", 30, false); len(got) != 0 {
		t.Fatalf("invalid fact still found: %v", texts(got))
	}
	if n, _ := s.FactCounts(ctx, []string{"ken"}); n["ken"] != 0 {
		t.Fatalf("count includes invalid fact: %v", n)
	}
	got, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "durian", 30, true)
	if len(got) != 1 || got[0].Valid() || !strings.Contains(got[0].String(), "no longer true since 2026-11-05: said he hates it now") {
		t.Fatalf("history: %v", texts(got))
	}
	s.AddPersonFacts(ctx, "ken", Source{Chat: "dm", Tier: TierSelf}, []string{"Likes: durian"})
	if got, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "durian", 30, false); len(got) != 1 {
		t.Fatalf("re-said fact not revived: %v", texts(got))
	}
}

// J4: a lower tier can't invalidate a higher one, and search ranks by tier
// and marks hearsay as unverified.
func TestTiers(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	s.AddPersonFacts(ctx, "ana", Source{Chat: "g", SaidBy: "budi", Tier: TierParticipant}, []string{"Job: dentist"})
	s.AddPersonFacts(ctx, "ana", Source{Chat: "g", SaidBy: "ana", Tier: TierSelf}, []string{"Job: nurse"})
	s.AddPersonFacts(ctx, "ana", Source{Chat: "g", Tier: TierInferred}, []string{"Job: maybe a vet"})
	got, _ := s.SearchPersonFacts(ctx, []string{"ana"}, "job", 30, false)
	want := []string{"Job: nurse", "Job: dentist [unverified]", "Job: maybe a vet [unverified]"}
	if strings.Join(texts(got), "|") != strings.Join(want, "|") {
		t.Fatalf("ranking:\n got %v\nwant %v", texts(got), want)
	}
	if ok, err := s.InvalidateFact(ctx, "ana", "Job: nurse", TierParticipant, "budi says no"); ok || err != ErrOutranked {
		t.Fatalf("participant invalidated a self fact: %v %v", ok, err)
	}
	if ok, err := s.InvalidateFact(ctx, "ana", "Job: dentist", TierSelf, "she says nurse"); !ok || err != nil {
		t.Fatalf("self could not invalidate hearsay: %v %v", ok, err)
	}
	// Re-said by a higher tier raises it.
	s.AddPersonFacts(ctx, "ana", Source{Chat: "g", Tier: TierOwner}, []string{"Job: maybe a vet"})
	if got, _ := s.SearchPersonFacts(ctx, []string{"ana"}, "vet", 30, false); len(got) != 1 || got[0].Tier != TierOwner {
		t.Fatalf("tier not raised: %+v", got)
	}
	// Global search ranks the same way.
	if got, _ := s.SearchAllFacts(ctx, "job", 30, false); len(got) != 2 || got[0].Tier != TierOwner {
		t.Fatalf("global: %v", texts(got))
	}
}

// J1: state facts expire after StateTTL; J4: identity claims below self are
// claims.
func TestKinds(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	s.AddPersonFacts(ctx, "ken", Source{Chat: "dm", Tier: TierSelf}, []string{"State: on the way home", "Alias: Kenny"})
	s.AddPersonFacts(ctx, "ana", Source{Chat: "g", Tier: TierInferred}, []string{"Alias: Wayan"})
	got, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "", 30, false)
	if len(got) != 2 || !strings.Contains(strings.Join(texts(got), "|"), "on the way home [state since "+now.Local().Format("Mon 15:04")+"]") {
		t.Fatalf("state: %v", texts(got))
	}
	*now = now.Add(StateTTL + time.Minute)
	if got, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "", 30, false); len(got) != 1 || got[0].Text != "Alias: Kenny" || got[0].Kind != KindFact {
		t.Fatalf("after expiry: %+v", got)
	}
	if got, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "home", 30, true); len(got) != 1 || got[0].Reason != "expired" {
		t.Fatalf("expired state history: %+v", got)
	}
	if got, _ := s.SearchPersonFacts(ctx, []string{"ana"}, "", 30, false); len(got) != 1 || got[0].Kind != KindClaim ||
		got[0].String() != "Alias: Wayan [claim, unverified]" {
		t.Fatalf("inferred alias: %v", texts(got))
	}
}

// J11: near-duplicate facts overwrite each other instead of piling up.
func TestNearDuplicatesOverwrite(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	inf := Source{Chat: "g", Tier: TierInferred}
	s.AddPersonFacts(ctx, "f", inf, []string{
		`Behavior: repeatedly demanded Rin say "banana" as bait`,
		`Behavior: spams "say banana" to provoke Rin`,
		`Behavior: escalates prayer requests from Hebrew to hieroglyphics to cuneiform`,
		`Behavior: escalates prayer requests to Chinese and Japanese`,
		`Behavior: quizzes assistant on factorials and pi digits`,
		"Alias: Joko", "Alias: Joko Santoso", // nicknames are never merged
		"Likes: chess", "Games: chess puzzles", // different labels
	})
	got, _ := s.SearchPersonFacts(ctx, []string{"f"}, "", 30, false)
	want := map[string]bool{
		`Behavior: spams "say banana" to provoke Rin`:                 true,
		`Behavior: escalates prayer requests to Chinese and Japanese`: true,
		`Behavior: quizzes assistant on factorials and pi digits`:     true,
		"Alias: Joko": true, "Alias: Joko Santoso": true, "Likes: chess": true, "Games: chess puzzles": true,
	}
	if len(got) != len(want) {
		t.Fatalf("got %d facts, want %d: %v", len(got), len(want), texts(got))
	}
	for _, f := range got {
		if !want[f.Text] {
			t.Fatalf("unexpected fact %q in %v", f.Text, texts(got))
		}
	}
	// A less trusted near-duplicate can't overwrite a more trusted fact.
	s.AddPersonFacts(ctx, "k", Source{Chat: "dm", Tier: TierSelf}, []string{"Job: IT engineer at a startup"})
	s.AddPersonFacts(ctx, "k", inf, []string{"Job: IT engineer, maybe a startup"})
	if got, _ := s.SearchPersonFacts(ctx, []string{"k"}, "", 30, false); len(got) != 1 || got[0].Text != "Job: IT engineer at a startup" {
		t.Fatalf("lower tier overwrote: %v", texts(got))
	}
	// An equal or more trusted one does, and several old copies collapse into it.
	s.AddPersonFacts(ctx, "k", Source{Chat: "dm", Tier: TierOwner}, []string{"Job: senior IT engineer at a startup"})
	if got, _ := s.SearchPersonFacts(ctx, []string{"k"}, "", 30, false); len(got) != 1 || got[0].Tier != TierOwner {
		t.Fatalf("owner did not overwrite: %+v", got)
	}
}

// K1: only valid owner-tier Prefers:/Rule: facts, newest first, capped.
func TestPreferences(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	owner := Source{Chat: "dm", SaidBy: "ken", Tier: TierOwner}
	s.AddPersonFacts(ctx, "ken", owner, []string{"Prefers: no time mentions unless decisive", "Likes: tea", "Rule: never call him bro"})
	s.AddPersonFacts(ctx, "ken", Source{Chat: "dm", Tier: TierInferred}, []string{"Prefers: long replies at night"})
	s.AddPersonFacts(ctx, "ana", owner, []string{"Prefers: English replies"})
	got, err := s.Preferences(ctx, "ken")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Rule: never call him bro", "Prefers: no time mentions unless decisive"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("got %q, want %q", got, want)
	}
	fs, _ := s.SearchPersonFacts(ctx, []string{"ken"}, "bro", 5, false)
	if len(fs) == 0 {
		t.Fatal("rule fact not found")
	}
	s.InvalidateFactID(ctx, fs[0].ID, TierOwner, "changed mind")
	if got, _ := s.Preferences(ctx, "ken"); len(got) != 1 {
		t.Fatalf("invalidated rule still shown: %q", got)
	}
	for _, p := range []string{"no emoji", "English replies", "short answers", "tea talk", "morning greetings", "cat pictures", "jazz songs"} {
		s.AddPersonFacts(ctx, "bo", owner, []string{"Prefers: " + p})
	}
	if got, _ := s.Preferences(ctx, "bo"); len(got) != MaxPreferences {
		t.Fatalf("got %d preferences, want cap %d", len(got), MaxPreferences)
	}
}

// K4: newest valid states first, capped, expired ones gone.
func TestRecentStates(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	src := Source{Chat: "dm", Tier: TierInferred}
	s.AddPersonFacts(ctx, "ken", src, []string{"State: up past midnight refusing to sleep"})
	*now = now.Add(25 * time.Hour) // the first one expires
	s.AddPersonFacts(ctx, "ken", src, []string{"State: throat better after warm water", "Likes: tea"})
	for i, st := range []string{"State: on the train to work", "State: woke at 4:58 and had breakfast", "State: deep in a code review"} {
		*now = now.Add(time.Duration(i+1) * time.Minute)
		s.AddPersonFacts(ctx, "ken", src, []string{st})
	}
	got, err := s.RecentStates(ctx, []string{"ken", "ana", ""})
	if err != nil {
		t.Fatal(err)
	}
	want := "State: deep in a code review|State: woke at 4:58 and had breakfast|State: on the train to work"
	var texts []string
	for _, f := range got["ken"] {
		texts = append(texts, f.Text)
	}
	if strings.Join(texts, "|") != want || got["ana"] != nil {
		t.Fatalf("got %q", texts)
	}
}
