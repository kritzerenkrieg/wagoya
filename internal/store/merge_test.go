package store

import (
	"context"
	"testing"
)

func TestMergeByNumber(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	s.UpsertPerson(ctx, "pn:6281000", "", "6281000", "Ana", *now)
	s.AddPersonFacts(ctx, "pn:6281000", Source{Chat: "g", Tier: TierSelf}, []string{"Job: nurse"})
	s.UpsertPerson(ctx, "lid99", "lid99", "6281000", "Ana", *now) // LID learned later
	if got, _ := s.SearchPersonFacts(ctx, []string{"lid99"}, "", 30, false); len(got) != 1 {
		t.Fatalf("facts not moved: %v", texts(got))
	}
	if n, _ := s.PersonName(ctx, "pn:6281000"); n != "" {
		t.Fatal("number profile not removed")
	}
}

func TestGuessMerge(t *testing.T) {
	s, now := openTest(t)
	ctx := context.Background()
	// Talked about first, so name-only profiles exist; then the accounts show up.
	for _, n := range []string{"Andi", "Nikolas", "Ken"} {
		if _, err := s.PersonByName(ctx, n, *now); err != nil {
			t.Fatal(err)
		}
	}
	s.UpsertPerson(ctx, "lidA", "lidA", "62811", "Andi Pratama", *now)
	s.UpsertPerson(ctx, "lidN", "lidN", "62822", "Niko", *now)
	s.UpsertPerson(ctx, "lidK", "lidK", "62833", "Ken", *now)
	s.UpsertPerson(ctx, "lidJ", "lidJ", "62844", "Kenji", *now)
	s.AddPersonFacts(ctx, "name:andi", Source{Chat: "g1", Tier: TierInferred}, []string{"Wants: burgundy phone"})
	s.AddPersonFacts(ctx, "name:ken", Source{Chat: "g2", Tier: TierInferred}, []string{"Likes: chess"})
	ps, err := s.NameProfiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inChat := func(chat string, a Account) bool {
		return chat == "g1" && a.Key == "lidA" || chat == "g2" && (a.Key == "lidK" || a.Key == "lidJ")
	}
	got := map[string]string{}
	for _, p := range ps {
		cands, _ := s.MergeCandidates(ctx, p.Name)
		if a, ok := PickMerge(p, cands, inChat); ok {
			got[p.Key] = a.Key
		}
	}
	// Andi: one match in the chat. Nikolas: no facts, one prefix match.
	// Ken: Ken and Kenji both match and both are in g2, so no guess.
	want := map[string]string{"name:andi": "lidA", "name:nikolas": "lidN"}
	if len(got) != len(want) || got["name:andi"] != "lidA" || got["name:nikolas"] != "lidN" {
		t.Fatalf("got %v, want %v", got, want)
	}
}
