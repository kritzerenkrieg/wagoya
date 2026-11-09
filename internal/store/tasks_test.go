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
	if err := s.AnswerTask(ctx, id, "use sqlite"); err != nil {
		t.Fatal(err)
	}
	c, ok, _ = s.ClaimTask(ctx, nil)
	if !ok || c.Action != "resume" || c.Task.SessionID != "sess" || c.Task.Text != "use sqlite" || c.Task.OwnerText != "use sqlite" {
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

// TestMigrateTasks: a database from before Q1 gets new_repo, and old rows
// read as not new.
func TestMigrateTasks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bot.db")
	s, err := Open(path, 16)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := s.db.Exec(`ALTER TABLE claude_tasks DROP COLUMN new_repo`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO claude_tasks(chat, requester, requester_name, owner_text, task, repo, status, created, updated)
		VALUES ('g','john','John','x','old task','wagoya','done',1,1)`); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path, 16); err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, ok, err := s.TaskByID(ctx, 1)
	if err != nil || !ok || task.NewRepo || task.Repo != "wagoya" {
		t.Fatalf("old task = %+v %v %v", task, ok, err)
	}
	id, _ := s.AddTask(ctx, Task{Chat: "g", Requester: "ana", Text: "new", Repo: "notes", NewRepo: true})
	if task, _, _ = s.TaskByID(ctx, id); !task.NewRepo {
		t.Fatalf("new task = %+v", task)
	}
}

// TestApprovals: a live prompt waits for the owner's answer, and blocked
// actions resume with their rules on a yes or end the task on a no (R4).
func TestApprovals(t *testing.T) {
	s, now := taskStore(t)
	ctx := context.Background()
	id, _ := s.AddTask(ctx, Task{Chat: "c", Requester: "own", OwnerText: "build it", Text: "Build it", Repo: "rvm"})
	s.ClaimTask(ctx, nil)

	// Live prompt: not answerable until announced, then taken once.
	if err := s.AskApproval(ctx, id, "Bash: cmake -S . -B build"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PendingApproval(ctx, "c", "own"); ok {
		t.Fatal("pending before it was announced")
	}
	if ts, _ := s.Unannounced(ctx); len(ts) != 1 || ts[0].Approval == "" || ts[0].Status != TaskRunning {
		t.Fatalf("unannounced = %+v", ts)
	}
	s.MarkAnnounced(ctx, id)
	if p, ok, _ := s.PendingApproval(ctx, "c", "own"); !ok || p.ID != id {
		t.Fatalf("pending = %+v %v", p, ok)
	}
	if _, ok, _ := s.PendingApproval(ctx, "c", "someone else"); ok {
		t.Fatal("another person's approval")
	}
	if a, _ := s.TakeApproval(ctx, id); a != "pending" {
		t.Fatalf("take before answer = %q", a)
	}
	if err := s.AnswerApproval(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := s.PendingApproval(ctx, "c", "own"); ok {
		t.Fatal("still pending after the answer")
	}
	if a, _ := s.TakeApproval(ctx, id); a != "allow" {
		t.Fatalf("take = %q", a)
	}
	if a, _ := s.TakeApproval(ctx, id); a != "pending" {
		t.Fatalf("answer taken twice: %q", a)
	}
	if err := s.AnswerApproval(ctx, id, true); err == nil {
		t.Fatal("answered with nothing asked")
	}

	// Blocked actions at the end of a run: yes resumes with the rules.
	if err := s.FinishTask(ctx, id, Report{Status: TaskNeedsApproval, SessionID: "sess", Result: "Found the build.",
		Approval: "- Bash: cmake -S . -B build", ApprovalRules: []string{"Bash(cmake -S . -B build)"}}); err != nil {
		t.Fatal(err)
	}
	s.MarkAnnounced(ctx, id)
	if p, ok, _ := s.PendingApproval(ctx, "c", "own"); !ok || p.Status != TaskNeedsApproval || len(p.ApprovalRules) != 1 {
		t.Fatalf("pending blocked = %+v %v", p, ok)
	}
	if err := s.AnswerApproval(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	c, ok, _ := s.ClaimTask(ctx, nil)
	if !ok || c.Action != "resume" || c.Task.ApprovalAnswer != "allow" || c.Task.ApprovalRules[0] != "Bash(cmake -S . -B build)" {
		t.Fatalf("resume claim = %+v", c)
	}
	if task, _, _ := s.TaskByID(ctx, id); task.Approval != "" || task.ApprovalRules != nil {
		t.Fatalf("approval not cleared on claim: %+v", task)
	}

	// No: the task ends as done, with its report.
	s.FinishTask(ctx, id, Report{Status: TaskNeedsApproval, Approval: "- Bash: git push", ApprovalRules: []string{"Bash(git push)"}})
	if err := s.AnswerApproval(ctx, id, false); err != nil {
		t.Fatal(err)
	}
	if task, _, _ := s.TaskByID(ctx, id); task.Status != TaskDone || task.Result != "Found the build." || task.Approval != "" {
		t.Fatalf("denied = %+v", task)
	}
	if err := s.FinishTask(ctx, id, Report{Status: TaskNeedsApproval}); err == nil {
		t.Fatal("needs_approval without an approval accepted")
	}

	// Unanswered for 24 hours: failed.
	id2, _ := s.AddTask(ctx, Task{Chat: "c", Requester: "own", Text: "Other", Repo: "rvm"})
	s.ClaimTask(ctx, nil)
	s.FinishTask(ctx, id2, Report{Status: TaskNeedsApproval, Approval: "- Bash: x", ApprovalRules: []string{"Bash(x)"}})
	*now = now.Add(25 * time.Hour)
	s.ExpireTasks(ctx)
	if task, _, _ := s.TaskByID(ctx, id2); task.Status != TaskFailed {
		t.Fatalf("expired = %+v", task)
	}
}
