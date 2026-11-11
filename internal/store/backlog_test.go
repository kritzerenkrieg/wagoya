package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// U1: tasks belong to their owner; nobody else can see or change them.
func TestBacklogOwnership(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "b.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	due := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	a, err := s.AddTodo(ctx, Todo{Owner: "ann", Name: "Fix bike", Description: "rear brake", Due: due})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTodo(ctx, Todo{Owner: "ann", Name: "Read book"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddTodo(ctx, Todo{Owner: "bob", Name: "Bob's secret"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OwnTodo(ctx, "bob", a); !errors.Is(err, ErrNotYourTodo) {
		t.Fatalf("bob read ann's task: %v", err)
	}
	done := TodoDone
	if _, err := s.UpdateTodo(ctx, "bob", a, TodoChange{Status: &done}); !errors.Is(err, ErrNotYourTodo) {
		t.Fatalf("bob changed ann's task: %v", err)
	}
	got, _ := s.ListTodos(ctx, "ann", TodoOpen, "", 10)
	if len(got) != 2 || got[0].ID != a || !got[0].Due.Equal(due) {
		t.Fatalf("ann's open tasks (due first): %+v", got)
	}
	if got, _ := s.ListTodos(ctx, "ann", "", "BRAKE", 10); len(got) != 1 || got[0].ID != a {
		t.Fatalf("search: %+v", got)
	}
	if got, _ := s.ListTodos(ctx, "", "", "", 10); len(got) != 0 {
		t.Fatalf("no owner sees nothing: %+v", got)
	}
	upd, err := s.UpdateTodo(ctx, "ann", a, TodoChange{Status: &done, ClearDue: true})
	if err != nil || upd.Status != TodoDone || upd.Done.IsZero() || !upd.Due.IsZero() {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if n, _ := s.CountOpenTodos(ctx, "ann"); n != 1 {
		t.Fatalf("open = %d", n)
	}
	if n, _ := s.CountOpenTodos(ctx, "bob"); n != 1 {
		t.Fatalf("bob open = %d", n)
	}
}

func TestTodoSourceAndPersonByPN(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "b.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	s.AddTodo(ctx, Todo{Owner: "ann", Name: "x", Source: "vikunja:7", Status: TodoDone})
	if ok, _ := s.HasTodoSource(ctx, "vikunja:7"); !ok {
		t.Fatal("source not found")
	}
	if ok, _ := s.HasTodoSource(ctx, "vikunja:8"); ok {
		t.Fatal("unexpected source")
	}
	if k, _ := s.PersonByPN(ctx, "628"); k != "pn:628" {
		t.Fatalf("unknown number: %q", k)
	}
	s.UpsertPerson(ctx, "lid9", "lid9", "628", "Ann", time.Now())
	if k, _ := s.PersonByPN(ctx, "628"); k != "lid9" {
		t.Fatalf("known number: %q", k)
	}
}

// U1: only the owner's valid facts about the old app are retired.
func TestRetireFactsMentioning(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "b.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	s.AddPersonFacts(ctx, "ann", Source{Tier: TierOwner}, []string{"Tool: Vikunja board at flow.relow.net", "Likes: tea"})
	s.AddPersonFacts(ctx, "bob", Source{Tier: TierOwner}, []string{"Uses: vikunja too"})
	n, err := s.RetireFactsMentioning(ctx, "ann", []string{"vikunja", "flow.relow.net"}, "moved")
	if err != nil || n != 1 {
		t.Fatalf("retired %d, %v", n, err)
	}
	got, _ := s.PersonFacts(ctx, []string{"ann", "bob"})
	if len(got["ann"]) != 1 || len(got["bob"]) != 1 {
		t.Fatalf("left: %v", got)
	}
}
