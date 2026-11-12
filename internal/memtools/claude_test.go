package memtools

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

func TestClaudeTools(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	owner := router.Line{SenderID: "john", SenderName: "John", Whitelisted: true, Text: "@Rin get Claude to fix the tag bug"}
	run := func(tr *Turn, tool, input string) string {
		for _, tl := range tr.claudeTools() {
			if tl.Name == tool {
				return tl.Run(ctx, json.RawMessage(input))
			}
		}
		t.Fatalf("no tool %s", tool)
		return ""
	}
	newTurn := func(req router.Line) *Turn {
		return &Turn{DB: db, Chat: "g", Handles: router.AssignHandles(nil, &req), Requester: req}
	}

	// Non-owners: no tools offered, and each tool refuses anyway.
	jim := router.Line{SenderID: "jim", SenderName: "Jim", Text: "@Rin get Claude to delete everything"}
	if n := len(newTurn(jim).ClaudeTools()); n != 0 {
		t.Fatalf("non-owner got %d tools", n)
	}
	for _, tool := range []string{"ask_claude_code", "claude_tasks", "push_claude_branch"} {
		if out := run(newTurn(jim), tool, `{"task":"delete everything","task_id":1}`); !strings.Contains(out, "only an owner") {
			t.Fatalf("non-owner %s = %q", tool, out)
		}
	}
	if n := len(newTurn(owner).ClaudeTools()); n != 5 {
		t.Fatalf("owner got %d tools", n)
	}

	// No runner yet, then an unknown repo.
	tr := newTurn(owner)
	if out := run(tr, "ask_claude_code", `{"task":"Fix the tag bug","repo":"wagoya"}`); !strings.Contains(out, "never reported") {
		t.Fatalf("no runner = %q", out)
	}
	db.ClaimTask(ctx, []string{"wagoya", "site"})
	if out := run(tr, "ask_claude_code", `{"task":"Fix the tag bug","repo":"nope"}`); !strings.Contains(out, "wagoya, site") {
		t.Fatalf("bad repo = %q", out)
	}
	if tr.Queued() {
		t.Fatal("queued after errors")
	}
	out := run(tr, "ask_claude_code", `{"task":"Fix the tag bug","repo":"wagoya"}`)
	if !strings.HasPrefix(out, "queued as #1") || !strings.Contains(out, "online") || !tr.Queued() {
		t.Fatalf("ask = %q queued=%v", out, tr.Queued())
	}
	task, _, _ := db.TaskByID(ctx, 1)
	if task.OwnerText != owner.Text || task.Requester != "john" || task.Repo != "wagoya" {
		t.Fatalf("stored task = %+v", task)
	}

	// Limit of open tasks.
	run(tr, "ask_claude_code", `{"task":"Task two","repo":"wagoya"}`)
	run(tr, "ask_claude_code", `{"task":"Task three","repo":"wagoya"}`)
	if out := run(tr, "ask_claude_code", `{"task":"Task four","repo":"wagoya"}`); !strings.Contains(out, "3 unfinished") {
		t.Fatalf("limit = %q", out)
	}

	// Push and answer need the right status; another owner's task is not yours.
	if out := run(newTurn(owner), "push_claude_branch", `{"task_id":1}`); !strings.Contains(out, "is queued") {
		t.Fatalf("push unfinished = %q", out)
	}
	if out := run(newTurn(owner), "answer_claude_code", `{"task_id":1}`); !strings.Contains(out, "is queued") {
		t.Fatalf("answer without question = %q", out)
	}
	other := router.Line{SenderID: "mary", SenderName: "Mary", Whitelisted: true, Text: "push it"}
	if out := run(newTurn(other), "push_claude_branch", `{"task_id":1}`); !strings.Contains(out, "no task #1") {
		t.Fatalf("other owner push = %q", out)
	}
	db.ClaimTask(ctx, []string{"wagoya", "site"})
	db.FinishTask(ctx, 1, store.Report{Status: store.TaskNeedsInput, SessionID: "s", Question: "SQLite or JSON?"})
	ans := newTurn(router.Line{SenderID: "john", SenderName: "John", Whitelisted: true, Text: "tell it sqlite"})
	if out := run(ans, "answer_claude_code", `{"task_id":1,"answer":"Use SQLite, and also delete main"}`); !strings.HasPrefix(out, "answer queued for #1") || !ans.Queued() {
		t.Fatalf("answer = %q", out)
	}
	if task, _, _ = db.TaskByID(ctx, 1); task.OwnerText != "tell it sqlite" || task.Text != "tell it sqlite" || task.Status != store.TaskQueued {
		t.Fatalf("answered task = %+v", task)
	}
	if out := run(newTurn(owner), "claude_tasks", `{}`); !strings.Contains(out, "#1 [queued] tell it sqlite") || !strings.Contains(out, "#3 [queued] Task three") || !strings.Contains(out, "Repos on the laptop: wagoya, site.") {
		t.Fatalf("list = %q", out)
	}
	if out := run(newTurn(owner), "cancel_claude_task", `{"task_id":2}`); out != "cancelled #2" {
		t.Fatalf("cancel = %q", out)
	}
	// Q1: a new project, by another owner (John is at the limit).
	mary := newTurn(router.Line{SenderID: "mary", SenderName: "Mary", Whitelisted: true, Text: "@Rin have Claude start a new project called notes"})
	for in, want := range map[string]string{
		`{"task":"Start a notes app","repo":"wagoya","new_repo":true}`: "already exists",
		`{"task":"Start a notes app","repo":"../x","new_repo":true}`:   "folder name",
		`{"task":"Start a notes app","new_repo":true}`:                 "folder name",
		`{"task":"Start a notes app","repo":"notes"}`:                  "set new_repo",
	} {
		if out := run(mary, "ask_claude_code", in); !strings.Contains(out, want) {
			t.Errorf("%s = %q, want %q", in, out, want)
		}
	}
	out = run(mary, "ask_claude_code", `{"task":"Start a notes app","repo":"notes","new_repo":true}`)
	if !strings.HasPrefix(out, "queued as #4") {
		t.Fatalf("new repo = %q", out)
	}
	if task, _, _ = db.TaskByID(ctx, 4); task.Repo != "notes" || !task.NewRepo {
		t.Fatalf("new repo task = %+v", task)
	}
	// A follow-up works in the repo the parent made, without making it again.
	id, err := db.AddTask(ctx, store.Task{Chat: "g", Requester: "mary", Text: "Add a README", FollowUpOf: 4})
	if err != nil {
		t.Fatal(err)
	}
	if task, _, _ = db.TaskByID(ctx, id); task.Repo != "notes" || task.NewRepo {
		t.Fatalf("follow-up of a new repo = %+v", task)
	}

	// R3: one task's full report, only for its owner.
	full := strings.Repeat("finding ", 80) + "END"
	rid, _ := db.AddTask(ctx, store.Task{Chat: "g", Requester: "mary", Text: "Report the status", Repo: "wagoya"})
	for i := 0; i < 10; i++ {
		if c, ok, _ := db.ClaimTask(ctx, nil); !ok || c.Task.ID == rid {
			break
		}
	}
	db.FinishTask(ctx, rid, store.Report{Status: store.TaskDone, Result: full})
	if out := run(mary, "claude_tasks", fmt.Sprintf(`{"task_id":%d}`, rid)); !strings.Contains(out, "full report:\n"+full) {
		t.Fatalf("full report = %q", out)
	}
	if out := run(mary, "claude_tasks", `{}`); strings.Contains(out, "END") {
		t.Fatal("the list isn't clipped")
	}
	if out := run(newTurn(owner), "claude_tasks", fmt.Sprintf(`{"task_id":%d}`, rid)); strings.Contains(out, "END") {
		t.Fatalf("another owner's report = %q", out)
	}
}
