package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritzerenkrieg/wagoya/internal/memtools"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

func TestTaskCommandRoundTrip(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bot.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cmd := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runTaskCommand(ctx, db, args, strings.NewReader(stdin), &out)
		return strings.TrimSpace(out.String()), err
	}
	if out, err := cmd("", "claim", "wagoya"); err != nil || out != "{}" {
		t.Fatalf("idle claim = %q %v", out, err)
	}
	id, _ := db.AddTask(ctx, store.Task{Chat: "g@g.us", Requester: "john", OwnerText: "fix it", Text: "Fix it", Repo: "wagoya"})
	out, err := cmd("", "claim", "wagoya")
	var c store.Claim
	if err != nil || json.Unmarshal([]byte(out), &c) != nil || c.Action != "run" || c.Task.ID != id || c.Task.OwnerText != "fix it" {
		t.Fatalf("claim = %q %v", out, err)
	}
	if out, _ := cmd("", "status", "1"); out != store.TaskRunning {
		t.Fatalf("status = %q", out)
	}
	if _, err := cmd(`{"status":"done","branch":"claude/task-1","result":"Fixed it."}`, "done", "1"); err != nil {
		t.Fatal(err)
	}
	got, _, _ := db.TaskByID(ctx, id)
	if got.Status != store.TaskDone || got.Result != "Fixed it." || got.Announced {
		t.Fatalf("after done = %+v", got)
	}
	if _, err := cmd(`{"status":"done"}`, "done", "1"); err == nil {
		t.Fatal("reported twice")
	}
	if _, err := cmd(`{"status":"merged"}`, "done", "1"); err == nil {
		t.Fatal("accepted a bad status")
	}
}

func TestTaskMessage(t *testing.T) {
	group := store.Task{ID: 3, Chat: "123@g.us", Requester: "john", Status: store.TaskDone}
	got := taskMessage(group, memtools.MarkCompleted+"\nDone, see @{jim}.")
	if got != memtools.MarkCompleted+"\n@{john} Done, see @ {jim}." {
		t.Fatalf("group message = %q", got)
	}
	send, _, ms := router.ExpandMentions(got, func(string) string { return "John" })
	if len(ms) != 1 || ms[0].Key != "john" || !strings.HasPrefix(send, memtools.MarkCompleted+"\n@john ") {
		t.Fatalf("expanded = %q %v", send, ms)
	}
	dm := store.Task{ID: 3, Chat: "123@s.whatsapp.net", Requester: "john", Status: store.TaskNeedsInput}
	if got := taskMessage(dm, "SQLite or JSON?"); got != memtools.MarkQuestion+"\nSQLite or JSON?" {
		t.Fatalf("dm question = %q", got)
	}
}

// TestApprovalCommands: the runner posts a live prompt and polls for the
// owner's answer over `wagoya task ask/answer` (R4).
func TestApprovalCommands(t *testing.T) {
	db, err := store.Open(filepath.Join(t.TempDir(), "bot.db"), 1)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	cmd := func(stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := runTaskCommand(ctx, db, args, strings.NewReader(stdin), &out)
		return strings.TrimSpace(out.String()), err
	}
	id, _ := db.AddTask(ctx, store.Task{Chat: "c@s.whatsapp.net", Requester: "john", Text: "Build", Repo: "rvm"})
	if _, err := cmd("Run `make`", "ask", "1"); err == nil {
		t.Fatal("asked on a queued task")
	}
	cmd("", "claim", "rvm")
	if _, err := cmd("Run `make`\n", "ask", "1"); err != nil {
		t.Fatal(err)
	}
	if out, _ := cmd("", "answer", "1"); out != "pending" {
		t.Fatalf("answer = %q", out)
	}
	db.MarkAnnounced(ctx, id)
	db.AnswerApproval(ctx, id, false)
	if out, _ := cmd("", "answer", "1"); out != "deny" {
		t.Fatalf("answer = %q", out)
	}
	db.CancelTask(ctx, id)
	if out, _ := cmd("", "answer", "1"); out != "deny" {
		t.Fatalf("answer after cancel = %q", out)
	}
}

func TestApprovalWord(t *testing.T) {
	for in, want := range map[string]int{ // 1 allow, 0 deny, -1 not an answer
		"yes": 1, "Yes.": 1, " OK! ": 1, "y": 1, "allow": 1, "okay": 1,
		"no": 0, "No.": 0, "n": 0, "deny": 0,
		"yes but wait": -1, "what is cmake?": -1, "sure": -1, "": -1, "yess": -1,
	} {
		allow, ok := approvalWord(in)
		got := -1
		if ok && allow {
			got = 1
		} else if ok {
			got = 0
		}
		if got != want {
			t.Errorf("approvalWord(%q) = %d, want %d", in, got, want)
		}
	}
}

func TestTaskMessageApprovals(t *testing.T) {
	for _, st := range []store.Task{
		{ID: 1, Chat: "c@s.whatsapp.net", Status: store.TaskRunning, Approval: "Run `make`"},
		{ID: 2, Chat: "c@s.whatsapp.net", Status: store.TaskNeedsApproval, Approval: "- Run `make`"},
	} {
		if got := taskMessage(st, "x"); !strings.HasPrefix(got, memtools.MarkQuestion+"\n") {
			t.Errorf("#%d message = %q", st.ID, got)
		}
	}
	if got := taskMessage(store.Task{ID: 3, Chat: "c@s.whatsapp.net", Status: store.TaskDone}, "x"); !strings.HasPrefix(got, memtools.MarkCompleted+"\n") {
		t.Errorf("done message = %q", got)
	}
}
