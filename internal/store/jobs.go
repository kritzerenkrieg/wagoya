package store

import (
	"context"
	"time"
)

// Scheduled messages (J7, J9, J10), kept in SQLite so they survive restarts.

const jobsSchema = `
CREATE TABLE IF NOT EXISTS jobs (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chat TEXT NOT NULL,
	created_by TEXT NOT NULL,      -- person key; '' for the bot's own idea
	created_name TEXT NOT NULL,
	self_initiated INTEGER NOT NULL,
	text TEXT NOT NULL,
	next_at INTEGER NOT NULL,
	repeat TEXT NOT NULL,          -- '', hourly, daily, weekdays, weekly
	created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS jobs_due ON jobs(next_at);
`

// Job is one scheduled message.
type Job struct {
	ID            int64
	Chat          string
	CreatedBy     string
	CreatedName   string
	SelfInitiated bool
	Text          string
	NextAt        time.Time
	Repeat        string
}

const jobCols = `id, chat, created_by, created_name, self_initiated, text, next_at, repeat`

func (s *Store) jobs(ctx context.Context, q string, args ...any) ([]Job, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+jobCols+` FROM jobs `+q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Job
	for rows.Next() {
		var j Job
		var at int64
		if err := rows.Scan(&j.ID, &j.Chat, &j.CreatedBy, &j.CreatedName, &j.SelfInitiated, &j.Text, &at, &j.Repeat); err != nil {
			return nil, err
		}
		j.NextAt = time.Unix(at, 0)
		out = append(out, j)
	}
	return out, rows.Err()
}

// AddJob stores a job and returns its id.
func (s *Store) AddJob(ctx context.Context, j Job) (int64, error) {
	res, err := s.db.ExecContext(ctx, `INSERT INTO jobs(chat, created_by, created_name, self_initiated, text, next_at, repeat, created_at)
		VALUES (?,?,?,?,?,?,?,?)`, j.Chat, j.CreatedBy, j.CreatedName, j.SelfInitiated, j.Text, j.NextAt.Unix(), j.Repeat, s.now().Unix())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// CountJobs is the number of pending jobs in chat.
func (s *Store) CountJobs(ctx context.Context, chat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM jobs WHERE chat = ?`, chat).Scan(&n)
	return n, err
}

// ListJobs returns the jobs of chat plus those created by creator anywhere,
// soonest first.
func (s *Store) ListJobs(ctx context.Context, chat, creator string) ([]Job, error) {
	return s.jobs(ctx, `WHERE chat = ? OR (created_by = ? AND ? != '') ORDER BY next_at`, chat, creator, creator)
}

// JobByID returns one job; ok is false if there is none.
func (s *Store) JobByID(ctx context.Context, id int64) (Job, bool, error) {
	got, err := s.jobs(ctx, `WHERE id = ?`, id)
	if err != nil || len(got) == 0 {
		return Job{}, false, err
	}
	return got[0], true, nil
}

// DeleteJob removes a job (cancelled, or a one-off that was sent).
func (s *Store) DeleteJob(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM jobs WHERE id = ?`, id)
	return err
}

// DueJobs returns jobs whose time has come.
func (s *Store) DueJobs(ctx context.Context, now time.Time) ([]Job, error) {
	return s.jobs(ctx, `WHERE next_at <= ? ORDER BY next_at`, now.Unix())
}

// RescheduleJob moves a recurring job to its next run.
func (s *Store) RescheduleJob(ctx context.Context, id int64, next time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE jobs SET next_at = ? WHERE id = ?`, next.Unix(), id)
	return err
}
