package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// O1-O5: Claude Code tasks, queued by Rin for an owner and run by a runner
// on the owner's laptop, which polls over SSH (`wagoya task ...`).
const tasksSchema = `
CREATE TABLE IF NOT EXISTS claude_tasks (
	id             INTEGER PRIMARY KEY AUTOINCREMENT,
	chat           TEXT NOT NULL,
	requester      TEXT NOT NULL,     -- person key of the owner
	requester_name TEXT NOT NULL,
	owner_text     TEXT NOT NULL,     -- the owner's own message, verbatim
	task           TEXT NOT NULL,     -- Rin's task, or her relay of an answer
	repo           TEXT NOT NULL,
	status         TEXT NOT NULL,
	follow_up_of   INTEGER NOT NULL DEFAULT 0,
	session_id     TEXT NOT NULL DEFAULT '',
	branch         TEXT NOT NULL DEFAULT '',
	question       TEXT NOT NULL DEFAULT '',
	result         TEXT NOT NULL DEFAULT '',
	created        INTEGER NOT NULL,
	updated        INTEGER NOT NULL,
	announced      INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS claude_tasks_status ON claude_tasks(status, updated);`

// Task statuses.
const (
	TaskQueued        = "queued"
	TaskRunning       = "running"
	TaskNeedsInput    = "needs_input"
	TaskDone          = "done"
	TaskFailed        = "failed"
	TaskCancelled     = "cancelled"
	TaskPushRequested = "push_requested"
	TaskPushing       = "pushing"
	TaskPushed        = "pushed"
	TaskPushFailed    = "push_failed"
)

// Limits and timeouts (O2).
const (
	MaxOpenTasks   = 3                // per owner
	TaskQueueTTL   = 24 * time.Hour   // queued or waiting on an answer
	TaskHeartbeat  = 5 * time.Minute  // a running task with no runner poll for this long failed
	TaskPushTTL    = 10 * time.Minute // a push claimed but not reported
	RunnerOnline   = 2 * time.Minute  // the runner polled within this: online
	FreshSessionAt = 7 * 24 * time.Hour
)

// openStatuses still have work ahead.
var openStatuses = []string{TaskQueued, TaskRunning, TaskNeedsInput, TaskPushRequested, TaskPushing}

// Task is one Claude Code task.
type Task struct {
	ID                                  int64
	Chat, Requester, RequesterName      string
	OwnerText, Text, Repo, Status       string
	FollowUpOf                          int64
	SessionID, Branch, Question, Result string
	Created, Updated                    time.Time
	Announced                           bool
}

const taskCols = `id, chat, requester, requester_name, owner_text, task, repo, status, follow_up_of, session_id, branch, question, result, created, updated, announced`

func scanTasks(rows *sql.Rows) ([]Task, error) {
	defer rows.Close()
	var out []Task
	for rows.Next() {
		var t Task
		var c, u int64
		if err := rows.Scan(&t.ID, &t.Chat, &t.Requester, &t.RequesterName, &t.OwnerText, &t.Text, &t.Repo, &t.Status,
			&t.FollowUpOf, &t.SessionID, &t.Branch, &t.Question, &t.Result, &c, &u, &t.Announced); err != nil {
			return nil, err
		}
		t.Created, t.Updated = time.Unix(c, 0), time.Unix(u, 0)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) tasks(ctx context.Context, q string, args ...any) ([]Task, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+taskCols+` FROM claude_tasks `+q, args...)
	if err != nil {
		return nil, err
	}
	return scanTasks(rows)
}

// TaskByID returns one task.
func (s *Store) TaskByID(ctx context.Context, id int64) (Task, bool, error) {
	ts, err := s.tasks(ctx, `WHERE id = ?`, id)
	if err != nil || len(ts) == 0 {
		return Task{}, false, err
	}
	return ts[0], true, nil
}

// OpenTasks counts requester's tasks that still have work ahead.
func (s *Store) OpenTasks(ctx context.Context, requester string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM claude_tasks WHERE requester = ? AND status IN (`+placeholders(len(openStatuses))+`)`,
		append([]any{requester}, anys(openStatuses)...)...).Scan(&n)
	return n, err
}

// AddTask queues t. A follow-up (t.FollowUpOf set) continues its parent's
// session and branch (O1); the caller checks the parent belongs to the owner.
func (s *Store) AddTask(ctx context.Context, t Task) (int64, error) {
	now := s.now().Unix()
	if t.FollowUpOf != 0 {
		p, ok, err := s.TaskByID(ctx, t.FollowUpOf)
		if err != nil {
			return 0, err
		}
		if !ok {
			return 0, fmt.Errorf("no task #%d", t.FollowUpOf)
		}
		t.Repo, t.Branch = p.Repo, p.Branch
		if s.now().Sub(p.Updated) < FreshSessionAt {
			t.SessionID = p.SessionID // older sessions may be gone from the laptop
		}
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO claude_tasks(chat, requester, requester_name, owner_text, task, repo, status, follow_up_of, session_id, branch, created, updated)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`, t.Chat, t.Requester, t.RequesterName, t.OwnerText, t.Text, t.Repo, TaskQueued, t.FollowUpOf, t.SessionID, t.Branch, now, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// AnswerTask queues an owner's answer to a task waiting on a question: the
// runner resumes the session with it (O1).
func (s *Store) AnswerTask(ctx context.Context, id int64, ownerText, answer string) error {
	return s.move(ctx, id, []string{TaskNeedsInput}, TaskQueued,
		`owner_text = ?, task = ?, question = ''`, ownerText, answer)
}

// RequestPush asks the runner to push a finished task's branch (O3).
func (s *Store) RequestPush(ctx context.Context, id int64) error {
	return s.move(ctx, id, []string{TaskDone, TaskPushFailed, TaskPushed}, TaskPushRequested, ``)
}

// CancelTask cancels a task that hasn't finished. A running one is stopped
// by the runner when it next checks the status.
func (s *Store) CancelTask(ctx context.Context, id int64) error {
	return s.move(ctx, id, []string{TaskQueued, TaskRunning, TaskNeedsInput, TaskPushRequested}, TaskCancelled, ``)
}

// move changes a task's status if it is in one of from, with extra SET
// assignments; it fails if the task isn't in one of them.
func (s *Store) move(ctx context.Context, id int64, from []string, to, set string, args ...any) error {
	if set != "" {
		set = ", " + set
	}
	q := `UPDATE claude_tasks SET status = ?, updated = ?, announced = 1` + set + ` WHERE id = ? AND status IN (` + placeholders(len(from)) + `)`
	all := append([]any{to, s.now().Unix()}, args...)
	all = append(all, id)
	all = append(all, anys(from)...)
	res, err := s.db.ExecContext(ctx, q, all...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		t, ok, _ := s.TaskByID(ctx, id)
		if !ok {
			return fmt.Errorf("no task #%d", id)
		}
		return fmt.Errorf("task #%d is %s", id, strings.ReplaceAll(t.Status, "_", " "))
	}
	return nil
}

// Claim is what the runner gets to do next.
type Claim struct {
	Action  string `json:"action"` // "run" (new session), "resume" or "push"
	Task    Task   `json:"task"`
	Context string `json:"context,omitempty"` // a follow-up's parent result, when its session is gone
}

// ClaimTask hands the runner the oldest piece of work, atomically, and
// records that it was seen with these repos (O2). ok is false when idle.
func (s *Store) ClaimTask(ctx context.Context, repos []string) (Claim, bool, error) {
	now := s.now()
	s.SetSetting(ctx, "runner_seen", strconv.FormatInt(now.Unix(), 10))
	s.SetSetting(ctx, "runner_repos", strings.Join(repos, ","))
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Claim{}, false, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+taskCols+` FROM claude_tasks WHERE status IN (?, ?) ORDER BY updated, id LIMIT 1`, TaskQueued, TaskPushRequested)
	if err != nil {
		return Claim{}, false, err
	}
	ts, err := scanTasks(rows)
	if err != nil || len(ts) == 0 {
		return Claim{}, false, err
	}
	t := ts[0]
	c := Claim{Task: t, Action: "run"}
	next := TaskRunning
	switch {
	case t.Status == TaskPushRequested:
		c.Action, next = "push", TaskPushing
	case t.SessionID != "":
		c.Action = "resume"
	case t.FollowUpOf != 0:
		var r string
		tx.QueryRowContext(ctx, `SELECT result FROM claude_tasks WHERE id = ?`, t.FollowUpOf).Scan(&r)
		c.Context = r
	}
	if _, err := tx.ExecContext(ctx, `UPDATE claude_tasks SET status = ?, updated = ? WHERE id = ?`, next, now.Unix(), t.ID); err != nil {
		return Claim{}, false, err
	}
	c.Task.Status = next
	return c, true, tx.Commit()
}

// Heartbeat records a runner poll on a running task and returns its status,
// so the runner stops when the owner cancels it.
func (s *Store) Heartbeat(ctx context.Context, id int64) (string, error) {
	now := s.now().Unix()
	s.SetSetting(ctx, "runner_seen", strconv.FormatInt(now, 10))
	s.db.ExecContext(ctx, `UPDATE claude_tasks SET updated = ? WHERE id = ? AND status IN (?, ?)`, now, id, TaskRunning, TaskPushing)
	t, ok, err := s.TaskByID(ctx, id)
	if err != nil || !ok {
		return "", errors.Join(err, fmt.Errorf("no task #%d", id))
	}
	return t.Status, nil
}

// Report is what the runner sends back when a run or push ends.
type Report struct {
	Status    string `json:"status"` // done, failed, needs_input, pushed, push_failed
	SessionID string `json:"session_id"`
	Branch    string `json:"branch"`
	Question  string `json:"question"`
	Result    string `json:"result"` // summary, commits, diff stat, refused actions
}

// FinishTask records a report on a running or pushing task and marks it for
// announcing. A cancelled task stays cancelled.
func (s *Store) FinishTask(ctx context.Context, id int64, r Report) error {
	switch r.Status {
	case TaskDone, TaskFailed, TaskNeedsInput, TaskPushed, TaskPushFailed:
	default:
		return fmt.Errorf("bad status %q", r.Status)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE claude_tasks SET status = ?, updated = ?, announced = 0,
		session_id = CASE WHEN ? != '' THEN ? ELSE session_id END,
		branch = CASE WHEN ? != '' THEN ? ELSE branch END,
		question = ?, result = CASE WHEN ? != '' THEN ? ELSE result END
		WHERE id = ? AND status IN (?, ?)`,
		r.Status, s.now().Unix(), r.SessionID, r.SessionID, r.Branch, r.Branch, r.Question, r.Result, r.Result, id, TaskRunning, TaskPushing)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("task #%d is not running", id)
	}
	return nil
}

// ExpireTasks fails work that waited or went silent too long, and marks it
// for announcing (O2). It returns how many changed.
func (s *Store) ExpireTasks(ctx context.Context) (int64, error) {
	now := s.now()
	var total int64
	for _, e := range []struct {
		status, to, why string
		after           time.Duration
	}{
		{TaskQueued, TaskFailed, "Never ran: the laptop runner didn't pick it up within 24 hours.", TaskQueueTTL},
		{TaskNeedsInput, TaskFailed, "Stopped: the question got no answer within 24 hours.", TaskQueueTTL},
		{TaskRunning, TaskFailed, "Stopped: the laptop runner went quiet in the middle of the task.", TaskHeartbeat},
		{TaskPushRequested, TaskPushFailed, "Push never ran: the laptop runner didn't pick it up within 24 hours.", TaskQueueTTL},
		{TaskPushing, TaskPushFailed, "Push didn't report back from the laptop.", TaskPushTTL},
	} {
		res, err := s.db.ExecContext(ctx, `UPDATE claude_tasks SET status = ?, result = ?, updated = ?, announced = 0 WHERE status = ? AND updated < ?`,
			e.to, e.why, now.Unix(), e.status, now.Add(-e.after).Unix())
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}

// Unannounced lists tasks whose new status Rin hasn't reported yet.
func (s *Store) Unannounced(ctx context.Context) ([]Task, error) {
	return s.tasks(ctx, `WHERE announced = 0 ORDER BY updated, id`)
}

// MarkAnnounced records that a task's status was reported.
func (s *Store) MarkAnnounced(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE claude_tasks SET announced = 1 WHERE id = ?`, id)
	return err
}

// RecentTasks lists requester's newest tasks.
func (s *Store) RecentTasks(ctx context.Context, requester string, limit int) ([]Task, error) {
	return s.tasks(ctx, `WHERE requester = ? ORDER BY id DESC LIMIT ?`, requester, limit)
}

// Runner reports when the laptop runner last polled and which repos it has.
func (s *Store) Runner(ctx context.Context) (seen time.Time, repos []string) {
	if v, _ := s.Setting(ctx, "runner_seen"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			seen = time.Unix(n, 0)
		}
	}
	if v, _ := s.Setting(ctx, "runner_repos"); v != "" {
		repos = strings.Split(v, ",")
	}
	return seen, repos
}

func placeholders(n int) string { return strings.TrimSuffix(strings.Repeat("?,", n), ",") }

func anys(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}
