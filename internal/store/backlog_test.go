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
	a, err := s.AddTodo(ctx, Todo{Owner: "ann", Name: "Fix bike", Description: "rear brake"})
	if err != nil {
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
	if got, _ := s.ListTodos(ctx, "ann", "", "BRAKE", 10); len(got) != 1 || got[0].ID != a || got[0].Status != TodoBacklog {
		t.Fatalf("search: %+v", got)
	}
	if got, _ := s.ListTodos(ctx, "", "", "", 10); len(got) != 0 {
		t.Fatalf("no owner sees nothing: %+v", got)
	}
	upd, err := s.UpdateTodo(ctx, "ann", a, TodoChange{Status: &done})
	if err != nil || upd.Status != TodoDone || upd.Done.IsZero() {
		t.Fatalf("update: %+v %v", upd, err)
	}
	if c, _ := s.TodoCounts(ctx, "ann"); len(c) != 0 {
		t.Fatalf("ann counts: %v", c)
	}
	if c, _ := s.TodoCounts(ctx, "bob"); c[TodoBacklog] != 1 {
		t.Fatalf("bob counts: %v", c)
	}
}

// U2: due time first (none last), then ongoing, hold, backlog; done last.
func TestBacklogOrder(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "b.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	now := time.Now()
	add := func(name, status string, due time.Time) {
		if _, err := s.AddTodo(ctx, Todo{Owner: "ann", Name: name, Status: status, Due: due}); err != nil {
			t.Fatal(err)
		}
	}
	add("backlog, no due", TodoBacklog, time.Time{})
	add("done", TodoDone, now.Add(-time.Hour))
	add("ongoing, no due", TodoOngoing, time.Time{})
	add("backlog, due later", TodoBacklog, now.Add(48*time.Hour))
	add("hold, no due", TodoHold, time.Time{})
	add("overdue", TodoOngoing, now.Add(-24*time.Hour))
	add("backlog, due soon", TodoBacklog, now.Add(2*time.Hour))
	want := []string{"overdue", "backlog, due soon", "backlog, due later", "ongoing, no due", "hold, no due", "backlog, no due", "done"}
	got, _ := s.ListTodos(ctx, "ann", "", "", 20)
	if len(got) != len(want) {
		t.Fatalf("got %d", len(got))
	}
	for i, td := range got {
		if td.Name != want[i] {
			t.Fatalf("position %d: %q, want %q", i, td.Name, want[i])
		}
	}
	if act, _ := s.ListTodos(ctx, "ann", TodoActive, "", 20); len(act) != 6 {
		t.Fatalf("active: %d", len(act))
	}
	if hold, _ := s.ListTodos(ctx, "ann", TodoHold, "", 20); len(hold) != 1 || hold[0].Name != "hold, no due" {
		t.Fatalf("hold: %+v", hold)
	}
}

// U2: open tasks from before become backlog, and an "[Ongoing]" tag in the
// name becomes the status.
func TestMigrateBacklogStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "b.db")
	s, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"[Ongoing] Setup camera", "[on hold] Trip", "Plain", "[Ongoing]"} {
		if _, err := s.db.Exec(`INSERT INTO backlog(owner, name, status, created_at, updated_at) VALUES ('ann', ?, 'open', 1, 1)`, n); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	if s, err = Open(path, 16); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, _ := s.ListTodos(context.Background(), "ann", "", "", 10)
	have := map[string]string{}
	for _, td := range got {
		have[td.Name] = td.Status
	}
	want := map[string]string{"Setup camera": TodoOngoing, "Trip": TodoHold, "Plain": TodoBacklog, "[Ongoing]": TodoBacklog}
	for n, st := range want {
		if have[n] != st {
			t.Errorf("%q: %q, want %q (all: %v)", n, have[n], st, have)
		}
	}
}
