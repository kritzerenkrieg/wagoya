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
	if !strings.HasPrefix(out, "added: task 1 | backlog | no category | due Fri 2026-11-06 18:00 (in 1d3h) | Fix bike | rear brake") {
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
		t.Fatalf("active after done: %s", out)
	}
	if out := ann("add_task", `{"name":"Trip","status":"on hold"}`); !strings.Contains(out, "| hold |") {
		t.Fatalf("add on hold: %s", out)
	}
	if out := ann("update_task", `{"id":2,"status":"ongoing"}`); !strings.Contains(out, "task 2 | ongoing |") {
		t.Fatalf("to ongoing: %s", out)
	}
	if out := ann("update_task", `{"id":2,"status":"later"}`); !strings.HasPrefix(out, "error") {
		t.Fatalf("bad status: %s", out)
	}
	if out := ann("list_tasks", `{"status":"ongoing"}`); !strings.HasPrefix(out, "task 2 | ongoing") {
		t.Fatalf("list ongoing: %s", out)
	}
	if out := ann("list_tasks", `{"status":"all","query":"brake"}`); !strings.Contains(out, "task 1 |") {
		t.Fatalf("search all: %s", out)
	}
	// U4: categories
	if out := ann("add_task", `{"name":"Paint","category":" Hobby "}`); !strings.Contains(out, "task 3 | backlog | hobby |") {
		t.Fatalf("add category: %s", out)
	}
	if out := ann("update_task", `{"id":2,"category":"Work"}`); !strings.Contains(out, "task 2 | ongoing | work |") {
		t.Fatalf("set category: %s", out)
	}
	out = ann("list_tasks", `{"category":"work"}`)
	if !strings.HasPrefix(out, "task 2 |") || strings.Contains(out, "task 3") || !strings.HasSuffix(out, "their categories: hobby, work") {
		t.Fatalf("filter work: %s", out)
	}
	if out := ann("update_task", `{"id":2,"category":"none"}`); !strings.Contains(out, "| no category |") {
		t.Fatalf("clear category: %s", out)
	}
	if out := ann("list_tasks", `{"category":"none"}`); !strings.HasPrefix(out, "task 2 |") || strings.Contains(out, "task 3") {
		t.Fatalf("filter none: %s", out)
	}
	if out := ann("add_task", `{"name":"x","category":"`+strings.Repeat("a", 31)+`"}`); !strings.HasPrefix(out, "error") {
		t.Fatalf("long category: %s", out)
	}
	if out := bob("list_tasks", `{"category":"hobby"}`); out != "no tasks" {
		t.Fatalf("bob sees ann's categories: %s", out)
	}
	if tools := (&Turn{DB: db, Handles: map[string]string{}}).BacklogTools(); tools != nil {
		t.Fatal("tools without a requester")
	}
}
