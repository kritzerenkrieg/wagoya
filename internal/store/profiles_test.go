package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// V1: profiles are replaced whole, found for the backfill, and move with a merge.
func TestProfiles(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		s.AddPersonFacts(ctx, "ann", Source{Tier: TierInferred}, []string{"Likes: thing " + string(rune('a'+i))})
	}
	s.AddPersonFacts(ctx, "bob", Source{Tier: TierInferred}, []string{"Likes: chess"})
	if got, _ := s.PeopleWithoutProfile(ctx, 5); len(got) != 1 || got[0] != "ann" {
		t.Fatalf("without profile: %v", got)
	}
	if p, _ := s.GetProfile(ctx, "ann"); p.Text != "" {
		t.Fatalf("empty: %+v", p)
	}
	s.SetProfile(ctx, "ann", "Ann v1", "first")
	s.SetProfile(ctx, "ann", "Ann v2", "new job")
	p, _ := s.GetProfile(ctx, "ann")
	if p.Text != "Ann v2" || p.Reason != "new job" || p.Created.IsZero() {
		t.Fatalf("replace: %+v", p)
	}
	if got, _ := s.PeopleWithoutProfile(ctx, 5); len(got) != 0 {
		t.Fatalf("still listed: %v", got)
	}
	if h, _ := s.HasProfiles(ctx, []string{"ann", "bob"}); !h["ann"] || h["bob"] {
		t.Fatalf("has: %v", h)
	}

	// H3 merge: a name-only profile moves to the real person if they have none.
	ghost, _ := s.PersonByName(ctx, "Yan", time.Now())
	s.SetProfile(ctx, ghost.Key, "Yan from futsal", "first")
	if ok, err := s.MergePerson(ctx, ghost.Key, "bob"); !ok || err != nil {
		t.Fatalf("merge: %v %v", ok, err)
	}
	if p, _ := s.GetProfile(ctx, "bob"); p.Text != "Yan from futsal" {
		t.Fatalf("moved: %+v", p)
	}
	if p, _ := s.GetProfile(ctx, ghost.Key); p.Text != "" {
		t.Fatalf("left behind: %+v", p)
	}
}
