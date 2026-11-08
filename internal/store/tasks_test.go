package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func taskStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "bot.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	now := time.Date(2026, 11, 8, 10, 0, 0, 0, time.Local)
	s.SetClock(func() time.Time { return now })
	return s, &now
}

func TestTaskLifecycle(t *testing.T) {
	s, now := taskStore(t)
	ctx := context.Background()
	id, err := s.AddTask(ctx, Task{Chat: "c", Requester: "own", RequesterName: "Owner", OwnerText: "fix it", Text: "Fix the bug", Repo: "wagoya"})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := s.OpenTasks(ctx, "own"); n != 1 {
		t.Fatalf("open = %d", n)
	}
	c, ok, err := s.ClaimTask(ctx, []string{"wagoya", "other"})
	if err != nil || !ok || c.Action != "run" || c.Task.ID != id || c.Task.Status != TaskRunning {
		t.Fatalf("claim = %+v %v %v", c, ok, err)
	}
	if _, ok, _ := s.ClaimTask(ctx, nil); ok {
		t.Fatal("claimed a running task twice")
	}
	seen, repos := s.Runner(ctx)
	if !seen.Equal(now.Truncate(time.Second)) || len(repos) != 0 {
		t.Fatalf("runner = %v %v", seen, repos) // the last claim reported no repos
	}
	if st, _ := s.Heartbeat(ctx, id); st != TaskRunning {
		t.Fatalf("heartbeat = %s", st)
	}
	if err := s.FinishTask(ctx, id, Report{Status: TaskNeedsInput, SessionID: "sess", Branch: "claude/task-1", Question: "SQLite or JSON?"}); err != nil {
		t.Fatal(err)
	}
	un, _ := s.Unannounced(ctx)
	if len(un) != 1 || un[0].Question != "SQLite or JSON?" {
		t.Fatalf("unannounced = %+v", un)
	}
	s.MarkAnnounced(ctx, id)
	if err := s.RequestPush(ctx, id); err == nil {
		t.Fatal("pushed a task waiting on a question")
	}
	if err := s.AnswerTask(ctx, id, "use sqlite", "Use SQLite."); err != nil {
		t.Fatal(err)
	}
	c, ok, _ = s.ClaimTask(ctx, nil)
	if !ok || c.Action != "resume" || c.Task.SessionID != "sess" || c.Task.Text != "Use SQLite." {
		t.Fatalf("resume claim = %+v", c)
	}
	s.FinishTask(ctx, id, Report{Status: TaskDone, Result: "Added X."})
	got, _, _ := s.TaskByID(ctx, id)
	if got.Status != TaskDone || got.Branch != "claude/task-1" || got.SessionID != "sess" || got.Question != "" {
		t.Fatalf("done = %+v", got)
	}

	// Push.
	if err := s.RequestPush(ctx, id); err != nil {
		t.Fatal(err)
	}
	c, ok, _ = s.ClaimTask(ctx, nil)
	if !ok || c.Action != "push" || c.Task.Status != TaskPushing {
		t.Fatalf("push claim = %+v", c)
	}
	s.FinishTask(ctx, id, Report{Status: TaskPushed})
	got, _, _ = s.TaskByID(ctx, id)
	if got.Status != TaskPushed || got.Result != "Added X." {
		t.Fatalf("pushed = %+v", got) // an empty report result keeps the old one
	}
	if n, _ := s.OpenTasks(ctx, "own"); n != 0 {
		t.Fatalf("open after push = %d", n)
	}

	// Follow-up continues the session and branch.
	fid, err := s.AddTask(ctx, Task{Chat: "c", Requester: "own", OwnerText: "add a test", Text: "Add a test", FollowUpOf: id})
	if err != nil {
		t.Fatal(err)
	}
	c, _, _ = s.ClaimTask(ctx, nil)
	if c.Task.ID != fid || c.Action != "resume" || c.Task.Branch != "claude/task-1" || c.Task.Repo != "wagoya" {
		t.Fatalf("follow-up claim = %+v", c)
	}
	// A cancelled running task can't be finished as done.
	if err := s.CancelTask(ctx, fid); err != nil {
		t.Fatal(err)
	}
	if st, _ := s.Heartbeat(ctx, fid); st != TaskCancelled {
		t.Fatalf("heartbeat after cancel = %s", st)
	}
	if err := s.FinishTask(ctx, fid, Report{Status: TaskDone}); err == nil {
		t.Fatal("finished a cancelled task")
	}
	if err := s.CancelTask(ctx, fid); err == nil || !strings.Contains(err.Error(), "cancelled") {
		t.Fatalf("cancel twice = %v", err)
	}
}

func TestTaskFollowUpOldSession(t *testing.T) {
	s, now := taskStore(t)
	ctx := context.Background()
	id, _ := s.AddTask(ctx, Task{Chat: "c", Requester: "own", OwnerText: "x", Text: "X", Repo: "wagoya"})
	s.ClaimTask(ctx, nil)
	s.FinishTask(ctx, id, Report{Status: TaskDone, SessionID: "old", Branch: "claude/task-1", Result: "Did X."})
	*now = now.Add(8 * 24 * time.Hour)
	s.AddTask(ctx, Task{Chat: "c", Requester: "own", OwnerText: "y", Text: "Y", FollowUpOf: id})
	c, _, _ := s.ClaimTask(ctx, nil)
	if c.Action != "run" || c.Task.SessionID != "" || c.Context != "Did X." || c.Task.Branch != "claude/task-1" {
		t.Fatalf("old follow-up claim = %+v", c)
	}
}

func TestExpireTasks(t *testing.T) {
	s, now := taskStore(t)
	ctx := context.Background()
	q, _ := s.AddTask(ctx, Task{Chat: "c", Requester: "own", OwnerText: "a", Text: "A", Repo: "r"})
	r, _ := s.AddTask(ctx, Task{Chat: "c", Requester: "own", OwnerText: "b", Text: "B", Repo: "r"})
	s.ClaimTask(ctx, nil) // claims q (oldest)
	*now = now.Add(TaskHeartbeat + time.Minute)
	if n, _ := s.ExpireTasks(ctx); n != 1 {
		t.Fatalf("expired %d, want the silent running task only", n)
	}
	got, _, _ := s.TaskByID(ctx, q)
	if got.Status != TaskFailed || got.Announced || !strings.Contains(got.Result, "quiet") {
		t.Fatalf("running expiry = %+v", got)
	}
	*now = now.Add(TaskQueueTTL)
	s.ExpireTasks(ctx)
	got, _, _ = s.TaskByID(ctx, r)
	if got.Status != TaskFailed || !strings.Contains(got.Result, "Never ran") {
		t.Fatalf("queued expiry = %+v", got)
	}
}
