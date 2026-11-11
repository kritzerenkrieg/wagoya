package store

import (
	"context"
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"time"
)

// The backlog (U1): each person's own tasks, kept here instead of an
// external app. A task belongs to its owner, and only they may see or
// change it; every query takes the owner.

const backlogSchema = `
CREATE TABLE IF NOT EXISTS backlog (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	owner TEXT NOT NULL,            -- person key
	name TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	due_at INTEGER NOT NULL DEFAULT 0,  -- unix seconds, 0 for none
	status TEXT NOT NULL DEFAULT 'backlog', -- backlog, ongoing, hold, done (U2)
	source TEXT NOT NULL DEFAULT '',     -- where a task came from, if not Rin
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	done_at INTEGER NOT NULL DEFAULT 0,
	chat TEXT NOT NULL DEFAULT '',            -- U3: where it was added, for a reminder
	due_set_at INTEGER NOT NULL DEFAULT 0,    -- U3: when the due time was last set
	reminded_due INTEGER NOT NULL DEFAULT 0   -- U3: the due time a reminder went out for
);
CREATE INDEX IF NOT EXISTS backlog_owner ON backlog(owner, status);
`

// Todo statuses (U2). A new task is in the backlog.
const (
	TodoBacklog = "backlog"
	TodoOngoing = "ongoing"
	TodoHold    = "hold"
	TodoDone    = "done"
)

// TodoStatuses are the statuses in list order.
var TodoStatuses = []string{TodoOngoing, TodoHold, TodoBacklog, TodoDone}

// TodoActive is a ListTodos filter: every task that isn't done.
const TodoActive = "active"

// ValidTodoStatus reports whether s is a status.
func ValidTodoStatus(s string) bool {
	for _, v := range TodoStatuses {
		if s == v {
			return true
		}
	}
	return false
}

// ErrNotYourTodo is returned for a task that doesn't exist or isn't the
// caller's; the two look the same, so nobody learns of others' tasks.
var ErrNotYourTodo = errors.New("no such task of yours")

// Todo is one backlog item.
type Todo struct {
	ID          int64
	Owner       string
	Name        string
	Description string
	Due         time.Time // zero for none
	Status      string
	Source      string
	Created     time.Time
	Updated     time.Time
	Done        time.Time // zero unless done
	Chat        string    // chat it was added in (U3)
	DueSet      time.Time // when Due was last set (U3); zero if never or unknown
	RemindedDue time.Time // the due time a reminder was sent for (U3)
}

// RemindAt is when a task's reminder goes out (U3): 09:00 the day before
// it is due, or an hour before if that had passed when the due time was
// set, or zero for none (no due time, or under an hour's notice).
func (t Todo) RemindAt() time.Time {
	if t.Due.IsZero() {
		return time.Time{}
	}
	d := t.Due.Local()
	if r := time.Date(d.Year(), d.Month(), d.Day()-1, 9, 0, 0, 0, d.Location()); t.DueSet.IsZero() || t.DueSet.Before(r) {
		return r
	}
	if h := t.Due.Add(-time.Hour); t.DueSet.Before(h) {
		return h
	}
	return time.Time{}
}

// TodoChange is an update; nil fields stay as they are. ClearDue removes
// the due time.
type TodoChange struct {
	Name, Description, Status *string
	Due                       *time.Time
	ClearDue                  bool
}

const todoCols = `id, owner, name, description, due_at, status, source, created_at, updated_at, done_at, chat, due_set_at, reminded_due`

func unixOrZero(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Unix()
}

func timeOrZero(n int64) time.Time {
	if n == 0 {
		return time.Time{}
	}
	return time.Unix(n, 0)
}

func (s *Store) todos(ctx context.Context, q string, args ...any) ([]Todo, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+todoCols+` FROM backlog `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Todo
	for rows.Next() {
		var t Todo
		var due, created, updated, done, dueSet, reminded int64
		if err := rows.Scan(&t.ID, &t.Owner, &t.Name, &t.Description, &due, &t.Status, &t.Source, &created, &updated, &done, &t.Chat, &dueSet, &reminded); err != nil {
			return nil, err
		}
		t.DueSet, t.RemindedDue = timeOrZero(dueSet), timeOrZero(reminded)
		t.Due, t.Created, t.Updated, t.Done = timeOrZero(due), time.Unix(created, 0), time.Unix(updated, 0), timeOrZero(done)
		out = append(out, t)
	}
	return out, rows.Err()
}

// AddTodo stores a task and returns its id. Created and Done are kept when
// set (an import), else now.
func (s *Store) AddTodo(ctx context.Context, t Todo) (int64, error) {
	if t.Owner == "" || strings.TrimSpace(t.Name) == "" {
		return 0, errors.New("task needs an owner and a name")
	}
	if t.Status == "" {
		t.Status = TodoBacklog
	}
	now := s.now()
	if t.Created.IsZero() {
		t.Created = now
	}
	if t.Status == TodoDone && t.Done.IsZero() {
		t.Done = now
	}
	if !t.Due.IsZero() && t.DueSet.IsZero() {
		t.DueSet = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO backlog(owner, name, description, due_at, status, source, created_at, updated_at, done_at, chat, due_set_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`, t.Owner, t.Name, t.Description, unixOrZero(t.Due), t.Status, t.Source,
		t.Created.Unix(), now.Unix(), unixOrZero(t.Done), t.Chat, unixOrZero(t.DueSet))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// OwnTodo returns owner's task id.
func (s *Store) OwnTodo(ctx context.Context, owner string, id int64) (Todo, error) {
	got, err := s.todos(ctx, `WHERE id = ? AND owner = ?`, id, owner)
	if err != nil {
		return Todo{}, err
	}
	if owner == "" || len(got) == 0 {
		return Todo{}, ErrNotYourTodo
	}
	return got[0], nil
}

// UpdateTodo changes owner's task id and returns it as it is now.
func (s *Store) UpdateTodo(ctx context.Context, owner string, id int64, c TodoChange) (Todo, error) {
	t, err := s.OwnTodo(ctx, owner, id)
	if err != nil {
		return Todo{}, err
	}
	if c.Name != nil {
		t.Name = *c.Name
	}
	if c.Description != nil {
		t.Description = *c.Description
	}
	now := s.now()
	if c.Due != nil && !c.Due.Equal(t.Due) {
		t.Due, t.DueSet = *c.Due, now // U3: a new due time gets its own reminder
	}
	if c.ClearDue {
		t.Due = time.Time{}
	}
	if c.Status != nil && *c.Status != t.Status {
		t.Status = *c.Status
		t.Done = time.Time{}
		if t.Status == TodoDone {
			t.Done = now
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE backlog SET name = ?, description = ?, due_at = ?, status = ?, updated_at = ?, done_at = ?, due_set_at = ?
		WHERE id = ? AND owner = ?`, t.Name, t.Description, unixOrZero(t.Due), t.Status, now.Unix(), unixOrZero(t.Done), unixOrZero(t.DueSet), id, owner)
	t.Updated = now
	return t, err
}

// ListTodos returns owner's tasks with status (a status, TodoActive for
// all but done, or "" for any), whose name or description contains query
// (any case; "" for all). U2: by due time, soonest first and none last,
// then ongoing, hold, backlog, then newest; done ones last, most recently
// done first.
func (s *Store) ListTodos(ctx context.Context, owner, status, query string, limit int) ([]Todo, error) {
	if owner == "" {
		return nil, nil
	}
	q := `WHERE owner = ?`
	args := []any{owner}
	switch status {
	case "":
	case TodoActive:
		q += ` AND status != 'done'`
	default:
		q += ` AND status = ?`
		args = append(args, status)
	}
	if query = strings.TrimSpace(query); query != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(query)) + "%"
		q += ` AND (lower(name) LIKE ? ESCAPE '\' OR lower(description) LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	q += ` ORDER BY status = 'done', CASE WHEN status = 'done' THEN -done_at ELSE 0 END, due_at = 0, due_at,
		CASE status WHEN 'ongoing' THEN 0 WHEN 'hold' THEN 1 ELSE 2 END, created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return s.todos(ctx, q, args...)
}

// TodoCounts is how many tasks owner has per status that isn't done.
func (s *Store) TodoCounts(ctx context.Context, owner string) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT status, count(*) FROM backlog WHERE owner = ? AND status != 'done' GROUP BY status`, owner)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}

var statusPrefix = regexp.MustCompile(`(?i)^\s*\[(ongoing|on hold|hold|backlog)\]\s*`)

// migrateBacklog moves tasks from before U2 to the new statuses: open
// becomes backlog, and a "[Ongoing]"-style tag at the start of a name
// becomes the status.
func migrateBacklog(db *sql.DB) error {
	for _, c := range [][2]string{ // U3
		{"chat", "TEXT NOT NULL DEFAULT ''"},
		{"due_set_at", "INTEGER NOT NULL DEFAULT 0"},
		{"reminded_due", "INTEGER NOT NULL DEFAULT 0"},
	} {
		var n int
		if err := db.QueryRow(`SELECT count(*) FROM pragma_table_info('backlog') WHERE name = ?`, c[0]).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			if _, err := db.Exec(`ALTER TABLE backlog ADD COLUMN ` + c[0] + ` ` + c[1]); err != nil {
				return err
			}
		}
	}
	if _, err := db.Exec(`UPDATE backlog SET status = 'backlog' WHERE status = 'open'`); err != nil {
		return err
	}
	rows, err := db.Query(`SELECT id, name FROM backlog WHERE name LIKE '[%' AND status != 'done'`)
	if err != nil {
		return err
	}
	type fix struct {
		id           int64
		name, status string
	}
	var fixes []fix
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if m := statusPrefix.FindStringSubmatch(name); m != nil {
			st := strings.ToLower(m[1])
			if st == "on hold" {
				st = TodoHold
			}
			if rest := strings.TrimSpace(name[len(m[0]):]); rest != "" {
				fixes = append(fixes, fix{id, rest, st})
			}
		}
	}
	rows.Close()
	for _, f := range fixes {
		if _, err := db.Exec(`UPDATE backlog SET name = ?, status = ? WHERE id = ?`, f.name, f.status, f.id); err != nil {
			return err
		}
	}
	return nil
}

// TodoRemindersDue returns the tasks whose reminder time has come (U3):
// not done, not yet due, and not reminded for this due time.
func (s *Store) TodoRemindersDue(ctx context.Context, now time.Time) ([]Todo, error) {
	got, err := s.todos(ctx, `WHERE status != 'done' AND due_at > ? AND reminded_due != due_at`, now.Unix())
	if err != nil {
		return nil, err
	}
	out := got[:0]
	for _, t := range got {
		if r := t.RemindAt(); !r.IsZero() && !r.After(now) {
			out = append(out, t)
		}
	}
	return out, nil
}

// MarkTodoReminded records that the reminder for task id's due time went
// out, so it is sent once.
func (s *Store) MarkTodoReminded(ctx context.Context, id int64, due time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE backlog SET reminded_due = ? WHERE id = ?`, due.Unix(), id)
	return err
}

// PersonAccount returns a profile's LID and number (U3: to reach their DM).
func (s *Store) PersonAccount(ctx context.Context, key string) (Account, error) {
	a := Account{Key: key}
	err := s.db.QueryRowContext(ctx, `SELECT name, lid, pn FROM people WHERE key = ?`, key).Scan(&a.Name, &a.LID, &a.PN)
	if errors.Is(err, sql.ErrNoRows) {
		if pn, ok := strings.CutPrefix(key, "pn:"); ok {
			a.PN = pn
		}
		return a, nil
	}
	return a, err
}
