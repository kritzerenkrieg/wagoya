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
	// U5: delete, own tasks only
	if out := bob("delete_task", `{"id":3}`); !strings.Contains(out, "no task with that id") {
		t.Fatalf("bob deleted ann's: %s", out)
	}
	if out := ann("delete_task", `{"id":3}`); !strings.HasPrefix(out, "deleted: task 3 |") {
		t.Fatalf("ann delete: %s", out)
	}
	if out := ann("list_tasks", `{"status":"all","query":"paint"}`); out != "no tasks" {
		t.Fatalf("deleted still listed: %s", out)
	}
	if out := ann("delete_task", `{"id":3}`); !strings.Contains(out, "no task with that id") {
		t.Fatalf("delete twice: %s", out)
	}
	if tools := (&Turn{DB: db, Handles: map[string]string{}}).BacklogTools(); tools != nil {
		t.Fatal("tools without a requester")
	}
}

// U6: the ready list groups by status, then category, with the same line
// layout every time.
func TestTaskListFormat(t *testing.T) {
	db := openDB(t)
	ann := backlogTurn(t, db, router.Line{SenderID: "ann", SenderName: "Ann"})
	for _, in := range []string{
		`{"name":"Report draft","status":"ongoing","category":"work","due":"2026-11-20 23:59"}`,
		`{"name":"Sketchbook","status":"ongoing","category":"hobby"}`,
		`{"name":"Vendor review","status":"hold","category":"work","due":"2026-11-04 23:59"}`,
		`{"name":"Client deck","category":"work"}`,
		`{"name":"Renew passport","category":"chore","due":"2027-01-03 21:45"}`,
		`{"name":"Dialogue UI","category":"Game dev"}`,
		`{"name":"Loose end"}`,
	} {
		if out := ann("add_task", in); !strings.HasPrefix(out, "added") {
			t.Fatal(out)
		}
	}
	out := ann("list_tasks", `{}`)
	want := `list for the reply (send as written):
Ongoing
Work
•  #1 Report draft: Fri 20 Nov 23:59

Hobby
•  #2 Sketchbook: no due time

Hold
Work
•  #3 Vendor review: Wed 04 Nov 23:59 (overdue)

Backlog
Chore
•  #5 Renew passport: Sun 03 Jan 2027 21:45

Game dev
•  #6 Dialogue UI

Work
•  #4 Client deck

Other
•  #7 Loose end
their categories:`
	if !strings.Contains(out, want) {
		t.Fatalf("list:\n%s", out)
	}
}

// U3: a task's reminder time carries its date, so the model doesn't pair the
// weekday with the due date (it said "Thu 27 Nov" for Thu 26 Nov).
func TestTaskReminderDate(t *testing.T) {
	ann := backlogTurn(t, openDB(t), router.Line{SenderID: "ann", SenderName: "Ann"})
	out := ann("add_task", `{"name":"Report","due":"2026-11-08 18:00"}`)
	if !strings.Contains(out, "due Sun 2026-11-08 18:00 (in 3d3h), reminder Sat 2026-11-07 09:00 |") {
		t.Fatalf("add: %s", out)
	}
}
