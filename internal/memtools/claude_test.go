package memtools

import (
	"context"
	"encoding/json"
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
	budi := router.Line{SenderID: "budi", SenderName: "Budi", Text: "@Rin get Claude to delete everything"}
	if n := len(newTurn(budi).ClaudeTools()); n != 0 {
		t.Fatalf("non-owner got %d tools", n)
	}
	for _, tool := range []string{"ask_claude_code", "claude_tasks", "push_claude_branch"} {
		if out := run(newTurn(budi), tool, `{"task":"delete everything","task_id":1}`); !strings.Contains(out, "only an owner") {
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
	if out := run(newTurn(owner), "answer_claude_code", `{"task_id":1,"answer":"yes"}`); !strings.Contains(out, "is queued") {
		t.Fatalf("answer without question = %q", out)
	}
	other := router.Line{SenderID: "ana", SenderName: "Ana", Whitelisted: true, Text: "push it"}
	if out := run(newTurn(other), "push_claude_branch", `{"task_id":1}`); !strings.Contains(out, "no task #1") {
		t.Fatalf("other owner push = %q", out)
	}
	db.ClaimTask(ctx, []string{"wagoya", "site"})
	db.FinishTask(ctx, 1, store.Report{Status: store.TaskNeedsInput, SessionID: "s", Question: "SQLite or JSON?"})
	ans := newTurn(router.Line{SenderID: "john", SenderName: "John", Whitelisted: true, Text: "tell it sqlite"})
	if out := run(ans, "answer_claude_code", `{"task_id":1,"answer":"Use SQLite."}`); !strings.HasPrefix(out, "answer queued for #1") || !ans.Queued() {
		t.Fatalf("answer = %q", out)
	}
	if task, _, _ = db.TaskByID(ctx, 1); task.OwnerText != "tell it sqlite" || task.Status != store.TaskQueued {
		t.Fatalf("answered task = %+v", task)
	}
	if out := run(newTurn(owner), "claude_tasks", `{}`); !strings.Contains(out, "#1 [queued] Use SQLite.") || !strings.Contains(out, "#3 [queued] Task three") {
		t.Fatalf("list = %q", out)
	}
	if out := run(newTurn(owner), "cancel_claude_task", `{"task_id":2}`); out != "cancelled #2" {
		t.Fatalf("cancel = %q", out)
	}
}

func TestWithMarker(t *testing.T) {
	for _, in := range []string{"Queued as #3.", MarkInitiated + "\nQueued as #3.", "  " + MarkInitiated + " Queued as #3."} {
		if got := WithMarker(MarkInitiated, in); got != MarkInitiated+"\nQueued as #3." {
			t.Errorf("WithMarker(%q) = %q", in, got)
		}
	}
}
