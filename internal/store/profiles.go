package store

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// Person profiles (V1): a prose picture of each person, written by the
// curator from what they say over time and rewritten only when there is a
// reason, so understanding grows with the chat instead of resting on the
// recent transcript and scattered one-line facts.

const profilesSchema = `
CREATE TABLE IF NOT EXISTS person_profiles (
	person TEXT PRIMARY KEY,         -- person key
	text TEXT NOT NULL,
	reason TEXT NOT NULL DEFAULT '', -- why it was last rewritten
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
`

// MaxProfileWords caps a profile (V1).
const MaxProfileWords = 400

// Profile is a person's profile.
type Profile struct {
	Person  string
	Text    string
	Reason  string
	Created time.Time
	Updated time.Time
}

// SetProfile stores (replaces) a person's profile.
func (s *Store) SetProfile(ctx context.Context, person, text, reason string) error {
	text = strings.TrimSpace(text)
	if person == "" || text == "" {
		return errors.New("profile needs a person and text")
	}
	now := s.now().Unix()
	_, err := s.db.ExecContext(ctx, `INSERT INTO person_profiles(person, text, reason, created_at, updated_at) VALUES (?,?,?,?,?)
		ON CONFLICT(person) DO UPDATE SET text = excluded.text, reason = excluded.reason, updated_at = excluded.updated_at`,
		person, text, strings.TrimSpace(reason), now, now)
	return err
}

// GetProfile returns a person's profile, or a zero Profile if none.
func (s *Store) GetProfile(ctx context.Context, person string) (Profile, error) {
	p := Profile{Person: person}
	var created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT text, reason, created_at, updated_at FROM person_profiles WHERE person = ?`, person).
		Scan(&p.Text, &p.Reason, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Profile{Person: person}, nil
	}
	if err != nil {
		return Profile{}, err
	}
	p.Created, p.Updated = time.Unix(created, 0), time.Unix(updated, 0)
	return p, nil
}

// HasProfiles reports which of keys have a profile.
func (s *Store) HasProfiles(ctx context.Context, keys []string) (map[string]bool, error) {
	out := map[string]bool{}
	for _, k := range keys {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM person_profiles WHERE person = ?`, k).Scan(&n); err != nil {
			return nil, err
		}
		out[k] = n > 0
	}
	return out, nil
}

// PeopleWithoutProfile returns people with at least minFacts valid facts and
// no profile, most facts first (V1: the one-time backfill).
func (s *Store) PeopleWithoutProfile(ctx context.Context, minFacts int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.person FROM person_facts f
		WHERE f.invalid_at IS NULL AND f.person NOT IN (SELECT person FROM person_profiles)
		GROUP BY f.person HAVING count(*) >= ? ORDER BY count(*) DESC, f.person`, minFacts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// mergeProfile moves a profile from one key to another inside a merge (H3):
// the destination's own profile wins; the source's is dropped either way.
func mergeProfile(ctx context.Context, tx *sql.Tx, from, to string) error {
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO person_profiles(person, text, reason, created_at, updated_at)
		SELECT ?, text, reason, created_at, updated_at FROM person_profiles WHERE person = ?`, to, from); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM person_profiles WHERE person = ?`, from)
	return err
}
