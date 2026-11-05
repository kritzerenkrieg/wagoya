// Package store keeps the recent chat window and the important-context
// memories in one SQLite file (B5, B11, B12, B16).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/kritzerenkrieg/wagoya/internal/router"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 (C4)
)

type Store struct {
	db           *sql.DB
	historyLimit int
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chat TEXT NOT NULL,
	ts INTEGER NOT NULL,
	sender_id TEXT NOT NULL,
	sender_name TEXT NOT NULL,
	whitelisted INTEGER NOT NULL,
	from_bot INTEGER NOT NULL,
	text TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS messages_chat ON messages(chat, id);
-- H1: how far the knowledge curator has read in each chat.
CREATE TABLE IF NOT EXISTS curator_state (
	chat TEXT PRIMARY KEY,
	last_id INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS memories (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	chat TEXT NOT NULL,
	category TEXT NOT NULL,
	text TEXT NOT NULL,
	ts INTEGER NOT NULL,
	UNIQUE(chat, text)
);
CREATE VIRTUAL TABLE IF NOT EXISTS memories_fts USING fts5(
	text, category UNINDEXED, chat UNINDEXED,
	content='memories', content_rowid='id'
);
CREATE TRIGGER IF NOT EXISTS memories_ai AFTER INSERT ON memories BEGIN
	INSERT INTO memories_fts(rowid, text, category, chat) VALUES (new.id, new.text, new.category, new.chat);
END;
-- D1: one global persona; every change is a new row, the latest wins.
CREATE TABLE IF NOT EXISTS persona (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	text TEXT NOT NULL,
	set_by TEXT NOT NULL,
	ts INTEGER NOT NULL
);
-- D3: global people profiles, keyed by LID ("pn:<number>" if no LID).
CREATE TABLE IF NOT EXISTS people (
	key TEXT PRIMARY KEY,
	lid TEXT NOT NULL DEFAULT '',
	pn TEXT NOT NULL DEFAULT '',
	name TEXT NOT NULL DEFAULT '',
	first_seen INTEGER NOT NULL,
	last_seen INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS person_facts (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	person TEXT NOT NULL REFERENCES people(key),
	text TEXT NOT NULL,
	chat TEXT NOT NULL,
	ts INTEGER NOT NULL,
	UNIQUE(person, text)
);
CREATE INDEX IF NOT EXISTS person_facts_person ON person_facts(person, id);
-- D7: facts have no cap; they are found by full-text search.
CREATE VIRTUAL TABLE IF NOT EXISTS person_facts_fts USING fts5(
	text, person UNINDEXED,
	content='person_facts', content_rowid='id', tokenize='porter unicode61'
);
CREATE TRIGGER IF NOT EXISTS person_facts_ai AFTER INSERT ON person_facts BEGIN
	INSERT INTO person_facts_fts(rowid, text, person) VALUES (new.id, new.text, new.person);
END;
CREATE TRIGGER IF NOT EXISTS person_facts_ad AFTER DELETE ON person_facts BEGIN
	INSERT INTO person_facts_fts(person_facts_fts, rowid, text, person) VALUES ('delete', old.id, old.text, old.person);
END;
CREATE TRIGGER IF NOT EXISTS person_facts_au AFTER UPDATE ON person_facts BEGIN
	INSERT INTO person_facts_fts(person_facts_fts, rowid, text, person) VALUES ('delete', old.id, old.text, old.person);
	INSERT INTO person_facts_fts(rowid, text, person) VALUES (new.id, new.text, new.person);
END;
`

// Open opens (or creates) the database at path.
func Open(path string, historyLimit int) (*Store, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Index facts written before the search index existed (cheap at this size).
	if _, err := db.Exec(`INSERT INTO person_facts_fts(person_facts_fts) VALUES ('rebuild')`); err != nil {
		db.Close()
		return nil, fmt.Errorf("rebuild fact index: %w", err)
	}
	return &Store{db: db, historyLimit: historyLimit}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// AddMessage stores a chat line and trims the chat to the last historyLimit
// messages (B5, B11).
func (s *Store) AddMessage(ctx context.Context, chat string, l router.Line) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(chat, ts, sender_id, sender_name, whitelisted, from_bot, text) VALUES (?,?,?,?,?,?,?)`,
		chat, l.Time.Unix(), l.SenderID, l.SenderName, l.Whitelisted, l.FromBot, l.Text)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE chat = ? AND id NOT IN (SELECT id FROM messages WHERE chat = ? ORDER BY id DESC LIMIT ?)`,
		chat, chat, s.historyLimit)
	return err
}

// History returns the stored window for chat, oldest first.
func (s *Store) History(ctx context.Context, chat string) ([]router.Line, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, sender_id, sender_name, whitelisted, from_bot, text FROM
		 (SELECT * FROM messages WHERE chat = ? ORDER BY id DESC LIMIT ?) ORDER BY id`,
		chat, s.historyLimit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []router.Line
	for rows.Next() {
		var l router.Line
		var ts int64
		if err := rows.Scan(&ts, &l.SenderID, &l.SenderName, &l.Whitelisted, &l.FromBot, &l.Text); err != nil {
			return nil, err
		}
		l.Time = time.Unix(ts, 0)
		out = append(out, l)
	}
	return out, rows.Err()
}

// AddMemories stores memories for chat, skipping exact duplicates. They are
// kept forever (B16).
func (s *Store) AddMemories(ctx context.Context, chat string, mems []router.Memory) error {
	for _, m := range mems {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO memories(chat, category, text, ts) VALUES (?,?,?,?)`,
			chat, m.Category, m.Text, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// SearchMemories returns up to limit memories of chat that match the words in
// query, best match first (B12). An empty query returns nothing.
func (s *Store) SearchMemories(ctx context.Context, chat, query string, limit int) ([]router.Memory, error) {
	q := ftsQuery(query)
	if q == "" {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT category, text FROM memories_fts WHERE memories_fts MATCH ? AND chat = ? ORDER BY bm25(memories_fts) LIMIT ?`,
		q, chat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []router.Memory
	for rows.Next() {
		var m router.Memory
		if err := rows.Scan(&m.Category, &m.Text); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// stopwords are question and filler words that would match almost anything.
var stopwords = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`the and for are was were has have had not but you your his her him she they them their
		its our this that these those with from what who whom whose where when why how which does did can could would should
		about know tell into than then there here just also any all some much many very yes yeah okay please
		yang dan ini itu apa siapa dimana kapan kenapa bagaimana dia aku saya kamu ada tidak bisa`) {
		stopwords[w] = true
	}
}

// ftsQuery turns free text into an FTS5 OR-query of quoted words, so user text
// can never inject FTS syntax.
func ftsQuery(text string) string {
	words := strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := map[string]bool{}
	var terms []string
	for _, w := range words {
		if len([]rune(w)) < 3 || seen[w] || stopwords[w] {
			continue
		}
		seen[w] = true
		terms = append(terms, `"`+w+`"`)
		if len(terms) == 24 {
			break
		}
	}
	return strings.Join(terms, " OR ")
}

// Persona returns the current persona, or "" if none was set (D1).
func (s *Store) Persona(ctx context.Context) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM persona ORDER BY id DESC LIMIT 1`).Scan(&text)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return text, err
}

// SetPersona stores a new persona; older versions stay as history (D1, D2).
func (s *Store) SetPersona(ctx context.Context, text, setBy string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO persona(text, set_by, ts) VALUES (?,?,?)`, text, setBy, time.Now().Unix())
	return err
}

// PersonaHistory returns up to limit personas, newest first.
func (s *Store) PersonaHistory(ctx context.Context, limit int) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT text FROM persona ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpsertPerson records that a person was seen, keeping their LID, number and
// latest display name (D3). Empty values never overwrite known ones.
func (s *Store) UpsertPerson(ctx context.Context, key, lid, pn, name string, seen time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO people(key, lid, pn, name, first_seen, last_seen) VALUES (?,?,?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET
			lid = CASE WHEN excluded.lid != '' THEN excluded.lid ELSE people.lid END,
			pn = CASE WHEN excluded.pn != '' THEN excluded.pn ELSE people.pn END,
			name = CASE WHEN excluded.name != '' THEN excluded.name ELSE people.name END,
			last_seen = MAX(people.last_seen, excluded.last_seen)`,
		key, lid, pn, name, seen.Unix(), seen.Unix())
	return err
}

// AddPersonFacts adds facts to a person's global profile, skipping exact
// duplicates; chat records where each fact was learned (D3, D4).
func (s *Store) AddPersonFacts(ctx context.Context, key, chat string, facts []string) error {
	for _, f := range facts {
		if _, err := s.db.ExecContext(ctx,
			`INSERT OR IGNORE INTO person_facts(person, text, chat, ts) VALUES (?,?,?,?)`,
			key, f, chat, time.Now().Unix()); err != nil {
			return err
		}
	}
	return nil
}

// ForgetPersonFact deletes a fact from a person's profile, matching text
// case-insensitively. It reports whether anything was deleted (D4).
func (s *Store) ForgetPersonFact(ctx context.Context, key, text string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM person_facts WHERE person = ? AND lower(trim(text)) = lower(trim(?))`, key, text)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// PersonFacts returns every fact for each key, oldest first (D7).
func (s *Store) PersonFacts(ctx context.Context, keys []string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, k := range keys {
		rows, err := s.db.QueryContext(ctx,
			`SELECT text FROM person_facts WHERE person = ? ORDER BY id`, k)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var t string
			if err := rows.Scan(&t); err != nil {
				rows.Close()
				return nil, err
			}
			out[k] = append(out[k], t)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// FactCounts returns how many facts each key has (D7).
func (s *Store) FactCounts(ctx context.Context, keys []string) (map[string]int, error) {
	out := map[string]int{}
	for _, k := range keys {
		var n int
		if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM person_facts WHERE person = ?`, k).Scan(&n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}

// Fact is one person fact with its owner's key.
type Fact struct {
	Person string
	Text   string
}

// SearchPersonFacts finds facts of the given people that match any word in
// query, best match first. An empty query lists their facts, newest first.
// Only keys passed in are searched, so absent people stay private (D7).
func (s *Store) SearchPersonFacts(ctx context.Context, keys []string, query string, limit int) ([]Fact, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+2)
	var q string
	if strings.TrimSpace(query) == "" {
		q = `SELECT person, text FROM person_facts WHERE person IN (` + in + `) ORDER BY id DESC LIMIT ?`
	} else {
		fq := ftsQuery(query)
		if fq == "" {
			return nil, nil
		}
		q = `SELECT person, text FROM person_facts_fts WHERE person_facts_fts MATCH ? AND person IN (` + in + `)
			ORDER BY bm25(person_facts_fts) LIMIT ?`
		args = append(args, fq)
	}
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fact
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.Person, &f.Text); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// PersonName returns the last display name seen for a person, or "".
func (s *Store) PersonName(ctx context.Context, key string) (string, error) {
	if key == "" {
		return "", nil
	}
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM people WHERE key = ?`, key).Scan(&name)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return name, err
}

// PersonRef is a known person: their profile key and last display name.
type PersonRef struct {
	Key  string
	Name string
}

// FindPeople finds known people by name, case-insensitively (D3, D7). A
// person matches when their name equals the query, contains it, or is a word
// of it ("Bapak Omar" finds "Omar"). Exact matches come first, then the
// most recently seen.
func (s *Store) FindPeople(ctx context.Context, name string, limit int) ([]PersonRef, error) {
	// "Omar #9249" picks one of several Omars by their ID tag.
	name, tag, tagged := strings.Cut(name, "#")
	tag = strings.ToLower(strings.TrimSpace(tag))
	if tagged && tag != "" {
		all, err := s.FindPeople(ctx, name, 50)
		var out []PersonRef
		for _, p := range all {
			if IDTag(p.Key) == tag {
				out = append(out, p)
			}
		}
		return out, err
	}
	q := strings.ToLower(strings.Join(strings.Fields(name), " "))
	if len([]rune(q)) < 2 {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT key, name FROM people
		WHERE (name != '' AND (lower(name) = ?1 OR (length(?1) >= 3 AND instr(lower(name), ?1) > 0)
			OR (length(name) >= 3 AND instr(' ' || ?1 || ' ', ' ' || lower(name) || ' ') > 0)))
			-- H3: nicknames saved as "Alias: <name>" facts
			OR key IN (SELECT person FROM person_facts WHERE lower(trim(text, ' .')) = 'alias: ' || ?1)
		ORDER BY lower(name) = ?1 DESC, last_seen DESC LIMIT ?2`, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PersonRef
	for rows.Next() {
		var p PersonRef
		if err := rows.Scan(&p.Key, &p.Name); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PersonByName returns the best-matching known person for name, creating a
// name-only profile ("name:<name>") when nobody matches, so facts can be
// saved about anyone (D4).
func (s *Store) PersonByName(ctx context.Context, name string, now time.Time) (PersonRef, error) {
	name = strings.Join(strings.Fields(name), " ")
	found, err := s.FindPeople(ctx, name, 1)
	if err != nil || len(found) > 0 {
		if len(found) > 0 {
			return found[0], nil
		}
		return PersonRef{}, err
	}
	if bare, _, tagged := strings.Cut(name, "#"); tagged {
		// Unknown tag: fall back to the name alone, never store "#..." in a name.
		return s.PersonByName(ctx, bare, now)
	}
	if name == "" {
		return PersonRef{}, fmt.Errorf("empty name")
	}
	p := PersonRef{Key: "name:" + strings.ToLower(name), Name: name}
	return p, s.UpsertPerson(ctx, p.Key, "", "", p.Name, now)
}

// SearchAllFacts searches every known person's facts (D7): full-text matches
// on fact text, plus the newest facts of people whose name matches a word of
// the query. An empty query lists the newest facts overall.
func (s *Store) SearchAllFacts(ctx context.Context, query string, limit int) ([]Fact, error) {
	var out []Fact
	seen := map[[2]string]bool{}
	add := func(rows *sql.Rows, err error) error {
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() && len(out) < limit {
			var f Fact
			if err := rows.Scan(&f.Person, &f.Text); err != nil {
				return err
			}
			if k := [2]string{f.Person, f.Text}; !seen[k] {
				seen[k] = true
				out = append(out, f)
			}
		}
		return rows.Err()
	}
	if strings.TrimSpace(query) == "" {
		err := add(s.db.QueryContext(ctx, `SELECT person, text FROM person_facts ORDER BY id DESC LIMIT ?`, limit))
		return out, err
	}
	// People named in the query first: "Omar" or "what does Omar do".
	for _, w := range strings.Fields(query) {
		w = strings.ToLower(strings.Trim(w, `?!.,:;"'`))
		if len([]rune(w)) < 3 || stopwords[w] {
			continue
		}
		people, err := s.FindPeople(ctx, w, 3)
		if err != nil {
			return nil, err
		}
		for _, p := range people {
			if err := add(s.db.QueryContext(ctx,
				`SELECT person, text FROM person_facts WHERE person = ? ORDER BY id DESC LIMIT ?`, p.Key, limit)); err != nil {
				return nil, err
			}
		}
	}
	if fq := ftsQuery(query); fq != "" {
		if err := add(s.db.QueryContext(ctx, `SELECT person, text FROM person_facts_fts WHERE person_facts_fts MATCH ?
			ORDER BY bm25(person_facts_fts) LIMIT ?`, fq, limit)); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// IDTag is a short tag that tells apart people with the same name: the last
// 4 digits of their LID or number, or "name" for a name-only profile.
func IDTag(key string) string {
	if strings.HasPrefix(key, "name:") {
		return "name"
	}
	d := strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, key)
	if len(d) > 4 {
		d = d[len(d)-4:]
	}
	return d
}

// NameTaken reports whether someone other than key has the same name
// (case-insensitive), so labels need an ID tag.
func (s *Store) NameTaken(ctx context.Context, name, key string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM people WHERE lower(name) = lower(?) AND key != ?`, name, key).Scan(&n)
	return n > 0, err
}

// TakeUnread returns the messages of chat the curator hasn't read yet, oldest
// first, and marks them read, but only once there are at least min of them
// (H1). Taking and marking happen in one transaction.
func (s *Store) TakeUnread(ctx context.Context, chat string, min int) ([]router.Line, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var last int64
	if err := tx.QueryRowContext(ctx, `SELECT last_id FROM curator_state WHERE chat = ?`, chat).Scan(&last); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx,
		`SELECT id, ts, sender_id, sender_name, whitelisted, from_bot, text FROM messages WHERE chat = ? AND id > ? ORDER BY id`, chat, last)
	if err != nil {
		return nil, err
	}
	var out []router.Line
	for rows.Next() {
		var l router.Line
		var ts int64
		if err := rows.Scan(&last, &ts, &l.SenderID, &l.SenderName, &l.Whitelisted, &l.FromBot, &l.Text); err != nil {
			rows.Close()
			return nil, err
		}
		l.Time = time.Unix(ts, 0)
		out = append(out, l)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 || len(out) < min {
		return nil, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO curator_state(chat, last_id) VALUES (?,?)
		ON CONFLICT(chat) DO UPDATE SET last_id = excluded.last_id`, chat, last); err != nil {
		return nil, err
	}
	return out, tx.Commit()
}

// UnreadCount is how many messages of chat the curator hasn't read (H1).
func (s *Store) UnreadCount(ctx context.Context, chat string) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM messages WHERE chat = ?
		AND id > coalesce((SELECT last_id FROM curator_state WHERE chat = ?), 0)`, chat, chat).Scan(&n)
	return n, err
}

// MergePerson moves a name-only profile's facts onto another profile and
// deletes it (H3). Only "name:" profiles can be merged away.
func (s *Store) MergePerson(ctx context.Context, from, to string) (bool, error) {
	if !strings.HasPrefix(from, "name:") || from == to || to == "" {
		return false, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM people WHERE key = ?`, from)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO person_facts(person, text, chat, ts)
		SELECT ?, text, chat, ts FROM person_facts WHERE person = ?`, to, from); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_facts WHERE person = ?`, from); err != nil {
		return false, err
	}
	return true, tx.Commit()
}
