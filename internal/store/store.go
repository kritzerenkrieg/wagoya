// Package store keeps the recent chat window and the important-context
// memories in one SQLite file (B5, B11, B12, B16).
package store

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/kritzerenkrieg/wagoya/internal/router"

	_ "modernc.org/sqlite" // pure-Go driver, CGO_ENABLED=0 (C4)
)

type Store struct {
	db           *sql.DB
	historyLimit int
	now          func() time.Time // clock for fact validity; tests replace it
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
CREATE INDEX IF NOT EXISTS messages_ts ON messages(ts);
-- Small runtime state, e.g. the learned bytes-per-token ratio (J5).
CREATE TABLE IF NOT EXISTS settings (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
-- J8: the curator's rolling summary of each chat.
CREATE TABLE IF NOT EXISTS chat_summary (
	chat TEXT PRIMARY KEY,
	text TEXT NOT NULL,
	ts INTEGER NOT NULL
);
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
	-- J1, J4: provenance and validity (migrateFacts adds these to old files).
	said_by TEXT NOT NULL DEFAULT '',
	tier TEXT NOT NULL DEFAULT 'legacy',
	kind TEXT NOT NULL DEFAULT 'fact',
	valid_from INTEGER NOT NULL DEFAULT 0,
	invalid_at INTEGER,
	invalid_reason TEXT NOT NULL DEFAULT '',
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
	if _, err := db.Exec(schema + jobsSchema + vectorsSchema + reactionsSchema + mutesSchema + tasksSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("schema: %w", err)
	}
	// Index facts written before the search index existed (cheap at this size).
	if _, err := db.Exec(`INSERT INTO person_facts_fts(person_facts_fts) VALUES ('rebuild')`); err != nil {
		db.Close()
		return nil, fmt.Errorf("rebuild fact index: %w", err)
	}
	// After the rebuild: the migration's updates fire the index triggers.
	if err := migrateFacts(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate facts: %w", err)
	}
	if err := migrateMessages(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate messages: %w", err)
	}
	if err := migrateJobs(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate jobs: %w", err)
	}
	if err := migrateTasks(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate tasks: %w", err)
	}
	return &Store{db: db, historyLimit: historyLimit, now: time.Now}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// MessageTTL is how long messages older than the window are kept, for the
// chat_history tool (L9). The window itself is always kept.
const MessageTTL = 72 * time.Hour

// AddMessage stores a chat line and drops messages that are both outside
// their chat's last historyLimit (B5, B11) and older than MessageTTL (L9),
// in every chat, with their reactions (L6).
func (s *Store) AddMessage(ctx context.Context, chat string, l router.Line) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO messages(chat, ts, sender_id, sender_name, whitelisted, from_bot, text, wa_id, to_bot, to_names) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		chat, l.Time.Unix(), l.SenderID, l.SenderName, l.Whitelisted, l.FromBot, l.Text, l.ID, l.ToBot, joinNames(l.To))
	if err != nil {
		return err
	}
	if _, err = s.db.ExecContext(ctx,
		`DELETE FROM messages WHERE ts < ? AND id < COALESCE(
		   (SELECT m.id FROM messages m WHERE m.chat = messages.chat ORDER BY m.id DESC LIMIT 1 OFFSET ?), -1)`,
		s.now().Add(-MessageTTL).Unix(), s.historyLimit+WindowBlock-1); err != nil {
		return err
	}
	// This chat: expired messages before its window go now (P6: the window
	// may be up to WindowBlock longer than historyLimit).
	if start, err := s.windowFrom(ctx, chat); err != nil {
		return err
	} else if _, err = s.db.ExecContext(ctx, `DELETE FROM messages WHERE chat = ? AND ts < ? AND id < ?`,
		chat, s.now().Add(-MessageTTL).Unix(), start); err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`DELETE FROM reactions WHERE NOT EXISTS (SELECT 1 FROM messages m WHERE m.chat = reactions.chat AND m.wa_id = reactions.msg_id)`)
	return err
}

// OlderMessages searches chat's messages from before the window (L9): those
// matching the most words of query first, then newest; an empty query pages
// back from the window. Results are oldest first.
func (s *Store) OlderMessages(ctx context.Context, chat, query string, limit, offset int) ([]router.Line, error) {
	start, err := s.windowStart(ctx, chat)
	if err != nil || start == 0 {
		return nil, err // empty chat
	}
	score, args := "0", []any{}
	for _, w := range queryWords(query) {
		pat := "%" + likeEscaper.Replace(w) + "%"
		score += ` + (text LIKE ? ESCAPE '\') + (sender_name LIKE ? ESCAPE '\')`
		args = append(args, pat, pat)
	}
	q := `SELECT ts, sender_id, sender_name, whitelisted, from_bot, text, wa_id FROM
	        (SELECT *, ` + score + ` AS score FROM messages WHERE chat = ? AND id < ?)`
	if len(args) > 0 {
		q += ` WHERE score > 0 ORDER BY score DESC, id DESC`
	} else {
		q += ` ORDER BY id DESC`
	}
	q += ` LIMIT ? OFFSET ?`
	args = append(args, chat, start, limit, offset)
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []router.Line
	for rows.Next() {
		var l router.Line
		var ts int64
		if err := rows.Scan(&ts, &l.SenderID, &l.SenderName, &l.Whitelisted, &l.FromBot, &l.Text, &l.ID); err != nil {
			return nil, err
		}
		l.Time = time.Unix(ts, 0)
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, rows.Err()
}

var likeEscaper = strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`)

// History returns the stored window for chat, oldest first.
func (s *Store) History(ctx context.Context, chat string) ([]router.Line, error) {
	start, err := s.windowStart(ctx, chat)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ts, sender_id, sender_name, whitelisted, from_bot, text, wa_id, to_bot, to_names FROM messages
		 WHERE chat = ? AND id >= ? ORDER BY id`, chat, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []router.Line
	for rows.Next() {
		var l router.Line
		var ts int64
		var to string
		if err := rows.Scan(&ts, &l.SenderID, &l.SenderName, &l.Whitelisted, &l.FromBot, &l.Text, &l.ID, &l.ToBot, &to); err != nil {
			return nil, err
		}
		l.Time, l.To = time.Unix(ts, 0), splitNames(to)
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	return out, s.loadReactions(ctx, chat, out)
}

// K7: addressee names are stored one per line.
func joinNames(ns []string) string {
	var out []string
	for _, n := range ns {
		if n = strings.Join(strings.Fields(n), " "); n != "" {
			out = append(out, n)
		}
	}
	return strings.Join(out, "\n")
}

func splitNames(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// WindowBlock is P6: the window shows historyLimit to historyLimit+WindowBlock
// lines and drops the oldest WindowBlock at once, so its first line, and the
// prompt prefix a provider can cache, stays put for WindowBlock messages.
const WindowBlock = 16

// windowStart is the id of the window's first message (0 for an empty
// chat). The start is kept in settings while at least historyLimit messages
// follow it and it is still among the last historyLimit+WindowBlock;
// otherwise it moves to show exactly the last historyLimit.
func (s *Store) windowStart(ctx context.Context, chat string) (int64, error) {
	start, err := s.windowFrom(ctx, chat)
	if err != nil || start == 0 {
		return 0, err
	}
	if err := s.SetSetting(ctx, "window:"+chat, strconv.FormatInt(start, 10)); err != nil {
		return 0, err
	}
	return start, nil
}

// windowFrom works out the window's first message id without saving it.
func (s *Store) windowFrom(ctx context.Context, chat string) (int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM messages WHERE chat = ? ORDER BY id DESC LIMIT ?`, chat, s.historyLimit+WindowBlock)
	if err != nil {
		return 0, err
	}
	var ids []int64 // newest first
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil || len(ids) == 0 {
		return 0, err
	}
	key := "window:" + chat
	if v, _ := s.Setting(ctx, key); v != "" {
		if kept, err := strconv.ParseInt(v, 10, 64); err == nil {
			for n, id := range ids {
				if id == kept && n+1 >= s.historyLimit {
					return kept, nil
				}
			}
		}
	}
	return ids[min(len(ids), s.historyLimit)-1], nil
}

// AddMemories stores memories for chat, skipping exact duplicates. They are
// kept forever (B16).
func (s *Store) AddMemories(ctx context.Context, chat string, mems []router.Memory) error {
	for _, m := range mems {
		if router.MergesIdentities(m.Text) { // K5
			log.Printf("dropped memory in %s: says two people are one (K5): %q", chat, m.Text)
			continue
		}
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
	terms := queryWords(text)
	for i, w := range terms {
		terms[i] = `"` + w + `"`
	}
	return strings.Join(terms, " OR ")
}

// queryWords are the distinct search words of text: lowercase, at least 3
// letters, no stopwords, at most 24.
func queryWords(text string) []string {
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
		terms = append(terms, w)
		if len(terms) == 24 {
			break
		}
	}
	return terms
}

// Setting returns a stored setting, or "".
func (s *Store) Setting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// SetSetting stores a setting.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES (?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}

// Summary returns the rolling summary of chat, or "" (J8).
func (s *Store) Summary(ctx context.Context, chat string) (string, error) {
	var text string
	err := s.db.QueryRowContext(ctx, `SELECT text FROM chat_summary WHERE chat = ?`, chat).Scan(&text)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return text, err
}

// SetSummary replaces the rolling summary of chat (J8).
func (s *Store) SetSummary(ctx context.Context, chat, text string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO chat_summary(chat, text, ts) VALUES (?,?,?)
		ON CONFLICT(chat) DO UPDATE SET text = excluded.text, ts = excluded.ts`, chat, text, s.now().Unix())
	return err
}

// RewriteSummaries applies f to every chat summary and saves the ones it
// changes (K6: scrubbing handles out of summaries written before the fix).
func (s *Store) RewriteSummaries(ctx context.Context, f func(string) string) (int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT chat, text FROM chat_summary`)
	if err != nil {
		return 0, err
	}
	changed := map[string]string{}
	for rows.Next() {
		var chat, text string
		if err := rows.Scan(&chat, &text); err != nil {
			rows.Close()
			return 0, err
		}
		if n := f(text); n != text {
			changed[chat] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for chat, text := range changed {
		if _, err := s.db.ExecContext(ctx, `UPDATE chat_summary SET text = ? WHERE chat = ?`, text, chat); err != nil {
			return 0, err
		}
	}
	return len(changed), nil
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
	if err != nil || lid == "" || pn == "" || key != lid {
		return err
	}
	// J12: a profile made when only their number was known is the same person.
	if ok, err := s.MergePerson(ctx, "pn:"+pn, lid); err != nil {
		return err
	} else if ok {
		log.Printf("merged pn:%s into %s (same number)", IDTag(pn), IDTag(lid))
	}
	return nil
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
		`SELECT id, ts, sender_id, sender_name, whitelisted, from_bot, text, to_bot, to_names FROM messages WHERE chat = ? AND id > ? ORDER BY id`, chat, last)
	if err != nil {
		return nil, err
	}
	var out []router.Line
	for rows.Next() {
		var l router.Line
		var ts int64
		var to string
		if err := rows.Scan(&last, &ts, &l.SenderID, &l.SenderName, &l.Whitelisted, &l.FromBot, &l.Text, &l.ToBot, &to); err != nil {
			rows.Close()
			return nil, err
		}
		l.Time, l.To = time.Unix(ts, 0), splitNames(to)
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

// ResetChat drops a chat's transcript and rolling summary (L2). Facts,
// memories and scheduled jobs stay. Message ids keep growing, so the
// curator's read position stays valid.
func (s *Store) ResetChat(ctx context.Context, chat string) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `DELETE FROM messages WHERE chat = ?`, chat)
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM reactions WHERE chat = ?`, chat); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chat_summary WHERE chat = ?`, chat); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, tx.Commit()
}
