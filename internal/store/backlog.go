package store

import (
	"context"
	"database/sql"
	"errors"
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
	status TEXT NOT NULL DEFAULT 'open', -- open, done
	source TEXT NOT NULL DEFAULT '',     -- e.g. "vikunja:12" for imported tasks
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	done_at INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS backlog_owner ON backlog(owner, status);
`

// Todo statuses.
const (
	TodoOpen = "open"
	TodoDone = "done"
)

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
}

// TodoChange is an update; nil fields stay as they are. ClearDue removes
// the due time.
type TodoChange struct {
	Name, Description, Status *string
	Due                       *time.Time
	ClearDue                  bool
}

const todoCols = `id, owner, name, description, due_at, status, source, created_at, updated_at, done_at`

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
		var due, created, updated, done int64
		if err := rows.Scan(&t.ID, &t.Owner, &t.Name, &t.Description, &due, &t.Status, &t.Source, &created, &updated, &done); err != nil {
			return nil, err
		}
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
		t.Status = TodoOpen
	}
	now := s.now()
	if t.Created.IsZero() {
		t.Created = now
	}
	if t.Status == TodoDone && t.Done.IsZero() {
		t.Done = now
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO backlog(owner, name, description, due_at, status, source, created_at, updated_at, done_at)
		VALUES (?,?,?,?,?,?,?,?,?)`, t.Owner, t.Name, t.Description, unixOrZero(t.Due), t.Status, t.Source,
		t.Created.Unix(), now.Unix(), unixOrZero(t.Done))
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
	if c.Due != nil {
		t.Due = *c.Due
	}
	if c.ClearDue {
		t.Due = time.Time{}
	}
	now := s.now()
	if c.Status != nil && *c.Status != t.Status {
		t.Status = *c.Status
		t.Done = time.Time{}
		if t.Status == TodoDone {
			t.Done = now
		}
	}
	_, err = s.db.ExecContext(ctx, `UPDATE backlog SET name = ?, description = ?, due_at = ?, status = ?, updated_at = ?, done_at = ?
		WHERE id = ? AND owner = ?`, t.Name, t.Description, unixOrZero(t.Due), t.Status, now.Unix(), unixOrZero(t.Done), id, owner)
	t.Updated = now
	return t, err
}

// ListTodos returns owner's tasks with status ("" for any), whose name or
// description contains query (any case; "" for all): open ones by due time
// (none last), then newest; done ones most recently done first.
func (s *Store) ListTodos(ctx context.Context, owner, status, query string, limit int) ([]Todo, error) {
	if owner == "" {
		return nil, nil
	}
	q := `WHERE owner = ?`
	args := []any{owner}
	if status != "" {
		q += ` AND status = ?`
		args = append(args, status)
	}
	if query = strings.TrimSpace(query); query != "" {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(query)) + "%"
		q += ` AND (lower(name) LIKE ? ESCAPE '\' OR lower(description) LIKE ? ESCAPE '\')`
		args = append(args, like, like)
	}
	q += ` ORDER BY status = 'done', CASE WHEN status = 'done' THEN -done_at ELSE 0 END, due_at = 0, due_at, created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	return s.todos(ctx, q, args...)
}

// CountOpenTodos is how many open tasks owner has.
func (s *Store) CountOpenTodos(ctx context.Context, owner string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM backlog WHERE owner = ? AND status = 'open'`, owner).Scan(&n)
	return n, err
}

// HasTodoSource reports whether a task with this source was already
// imported, so an import can run twice.
func (s *Store) HasTodoSource(ctx context.Context, source string) (bool, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM backlog WHERE source = ? LIMIT 1`, source).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// PersonByPN returns the profile key for a phone number (digits): the LID
// profile if known, else "pn:<number>".
func (s *Store) PersonByPN(ctx context.Context, pn string) (string, error) {
	var key string
	err := s.db.QueryRowContext(ctx, `SELECT key FROM people WHERE pn = ? ORDER BY key LIKE 'pn:%' LIMIT 1`, pn).Scan(&key)
	if errors.Is(err, sql.ErrNoRows) {
		return "pn:" + pn, nil
	}
	return key, err
}

// RetireFactsMentioning marks key's valid facts that mention any of words
// (any case) as outdated, kept as history like any invalidated fact (U1:
// facts that point at the old task app). It returns how many.
func (s *Store) RetireFactsMentioning(ctx context.Context, key string, words []string, reason string) (int64, error) {
	var total int64
	for _, w := range words {
		like := "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(strings.ToLower(w)) + "%"
		res, err := s.db.ExecContext(ctx, `UPDATE person_facts SET invalid_at = ?, invalid_reason = ?
			WHERE person = ? AND invalid_at IS NULL AND lower(text) LIKE ? ESCAPE '\'`, s.now().Unix(), reason, key, like)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
	}
	return total, nil
}
