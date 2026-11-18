package store

import (
	"context"
	"database/sql"
	"time"
)

// M2, M3: mutes and timeouts per person, and model spend per day.
const mutesSchema = `
CREATE TABLE IF NOT EXISTS mutes (
	key    TEXT PRIMARY KEY,  -- person key (LID or pn:number)
	name   TEXT NOT NULL,
	until  INTEGER NOT NULL,  -- unix seconds; 0 = until unmuted
	reason TEXT NOT NULL      -- "owner" (M2) or "flood" (M1)
);
CREATE TABLE IF NOT EXISTS spend (
	day TEXT PRIMARY KEY,     -- local date, 2006-01-02
	usd REAL NOT NULL
);`

// Mute is an active mute or timeout.
type Mute struct {
	Key, Name, Reason string
	Until             time.Time // zero: until unmuted
}

// SetMute mutes key until until (zero: until unmuted), replacing any mute.
func (s *Store) SetMute(ctx context.Context, key, name string, until time.Time, reason string) error {
	u := int64(0)
	if !until.IsZero() {
		u = until.Unix()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO mutes(key, name, until, reason) VALUES (?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET name = excluded.name, until = excluded.until, reason = excluded.reason`, key, name, u, reason)
	return err
}

// Unmute lifts key's mute or timeout and reports whether one was active.
func (s *Store) Unmute(ctx context.Context, key string) (bool, error) {
	active, err := s.Muted(ctx, key)
	if err != nil {
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `DELETE FROM mutes WHERE key = ?`, key)
	return active, err
}

// Muted reports whether key is muted or timed out now.
func (s *Store) Muted(ctx context.Context, key string) (bool, error) {
	var u int64
	err := s.db.QueryRowContext(ctx, `SELECT until FROM mutes WHERE key = ?`, key).Scan(&u)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return u == 0 || u > s.now().Unix(), nil
}

// Mutes lists the active mutes and timeouts, soonest to end first.
func (s *Store) Mutes(ctx context.Context) ([]Mute, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, name, until, reason FROM mutes WHERE until = 0 OR until > ? ORDER BY until = 0, until`, s.now().Unix())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Mute
	for rows.Next() {
		var m Mute
		var u int64
		if err := rows.Scan(&m.Key, &m.Name, &u, &m.Reason); err != nil {
			return nil, err
		}
		if u != 0 {
			m.Until = time.Unix(u, 0)
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// AddSpend adds usd to day's model spend and returns the day's total (M3).
func (s *Store) AddSpend(ctx context.Context, day string, usd float64) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx, `INSERT INTO spend(day, usd) VALUES (?, ?)
		ON CONFLICT(day) DO UPDATE SET usd = usd + excluded.usd RETURNING usd`, day, usd).Scan(&total)
	return total, err
}

// Spend is day's model spend so far.
func (s *Store) Spend(ctx context.Context, day string) (float64, error) {
	var total float64
	err := s.db.QueryRowContext(ctx, `SELECT usd FROM spend WHERE day = ?`, day).Scan(&total)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return total, err
}
