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
	got := taskMessage(group, memtools.MarkCompleted+"\nDone, see @{budi}.")
	if got != memtools.MarkCompleted+"\n@{john} Done, see @ {budi}." {
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
