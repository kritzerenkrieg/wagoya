package memtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

func backlogTurn(t *testing.T, db *store.Store, req router.Line) func(tool, input string) string {
	t.Helper()
	tr := &Turn{DB: db, Chat: "g@g.us", Handles: router.AssignHandles(nil, &req), Requester: req, Now: func() time.Time { return clock }}
	tools := tr.BacklogTools()
	return func(tool, input string) string {
		for _, tl := range tools {
			if tl.Name == tool {
				return tl.Run(context.Background(), json.RawMessage(input))
			}
		}
		t.Fatalf("no tool %s", tool)
		return ""
	}
}

// U1: anyone keeps tasks, but only their own: another person's task can't be
// listed, changed or even confirmed to exist.
func TestBacklogTools(t *testing.T) {
	db := openDB(t)
	ann := backlogTurn(t, db, router.Line{SenderID: "ann", SenderName: "Ann"})
	bob := backlogTurn(t, db, router.Line{SenderID: "bob", SenderName: "Bob"})

	out := ann("add_task", `{"name":"Fix bike","description":"rear brake","due":"2026-11-06 18:00"}`)
	if !strings.HasPrefix(out, "added: task 1 | open | due Fri 2026-11-06 18:00 (in 1d3h) | Fix bike | rear brake") {
		t.Fatalf("add: %s", out)
	}
	if out := ann("add_task", `{"name":"  "}`); !strings.HasPrefix(out, "error") {
		t.Fatalf("empty name: %s", out)
	}
	if out := bob("list_tasks", `{}`); out != "no tasks" {
		t.Fatalf("bob sees ann's: %s", out)
	}
	if out := bob("update_task", `{"id":1,"status":"done"}`); !strings.Contains(out, "no task with that id") {
		t.Fatalf("bob changed ann's: %s", out)
	}
	if out := ann("update_task", `{"id":1,"status":"done","due":"none"}`); !strings.HasPrefix(out, "updated: task 1 | done ") || !strings.Contains(out, "| no due time | Fix bike") {
		t.Fatalf("ann update: %s", out)
	}
	if out := ann("list_tasks", `{}`); out != "no tasks" {
		t.Fatalf("open after done: %s", out)
	}
	if out := ann("list_tasks", `{"status":"all","query":"brake"}`); !strings.Contains(out, "task 1 |") {
		t.Fatalf("search all: %s", out)
	}
	if tools := (&Turn{DB: db, Handles: map[string]string{}}).BacklogTools(); tools != nil {
		t.Fatal("tools without a requester")
	}
}
