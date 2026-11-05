package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode"
)

// Fact schema v2 (J1, J4): every person fact records who said it, how much
// that source is trusted (tier) and what sort of statement it is (kind).
// Outdated facts are invalidated, not deleted, and stay as history.

// Tier is how much a fact's source is trusted, highest first (J4).
type Tier string

const (
	TierOwner       Tier = "owner"       // a whitelisted person said it
	TierSelf        Tier = "self"        // the person said it about themselves
	TierParticipant Tier = "participant" // someone else in the chat said it
	TierInferred    Tier = "inferred"    // the curator's reading of the chat
	TierLegacy      Tier = "legacy"      // saved before J1/J4, source unknown
)

func (t Tier) rank() int {
	switch t {
	case TierOwner:
		return 5
	case TierSelf:
		return 4
	case TierParticipant:
		return 3
	case TierInferred:
		return 2
	}
	return 1
}

// rankSQL is Tier.rank as SQL over column col.
func rankSQL(col string) string {
	return fmt.Sprintf(`(CASE %s WHEN 'owner' THEN 5 WHEN 'self' THEN 4 WHEN 'participant' THEN 3 WHEN 'inferred' THEN 2 ELSE 1 END)`, col)
}

// Kind is what sort of statement a fact is (J1).
type Kind string

const (
	KindFact  Kind = "fact"
	KindState Kind = "state" // short-lived ("on the way home"); expires after StateTTL
	KindClaim Kind = "claim" // unverified talk, hearsay, identity claims
	KindJoke  Kind = "joke"
)

// StateTTL is how long a state fact stays valid (J1).
const StateTTL = 24 * time.Hour

// ClassifyKind derives a fact's kind from its label ("Claim: ...") and tier.
// A claim about identity (an alias) from anyone below self is a claim (J4).
func ClassifyKind(text string, tier Tier) Kind {
	label, _, ok := strings.Cut(text, ":")
	if !ok || len(label) > 20 {
		return KindFact
	}
	switch strings.ToLower(strings.TrimSpace(label)) {
	case "claim", "claims", "rumor", "rumour", "gossip", "behavior", "behaviour":
		return KindClaim
	case "joke", "jokes", "banter":
		return KindJoke
	case "state", "now", "status", "mood", "currently", "doing":
		return KindState
	case "alias", "identity", "same person":
		if tier.rank() < TierSelf.rank() {
			return KindClaim
		}
	}
	return KindFact
}

// Source says where new facts come from.
type Source struct {
	Chat   string
	SaidBy string // person key of who said it, "" if unknown
	Tier   Tier
	Kind   Kind // optional; otherwise read from the label (ClassifyKind)
}

// Fact is one stored person fact.
type Fact struct {
	ID        int64
	Person    string
	Text      string
	Chat      string
	SaidBy    string
	Tier      Tier
	Kind      Kind
	ValidFrom time.Time
	InvalidAt time.Time // zero while valid
	Reason    string    // why it was invalidated
}

// Valid reports whether the fact still holds.
func (f Fact) Valid() bool { return f.InvalidAt.IsZero() }

// Unverified reports whether the fact should be shown as unverified: hearsay,
// the curator's reading, or a claim or joke (J4).
func (f Fact) Unverified() bool {
	return f.Tier == TierParticipant || f.Tier == TierInferred || f.Kind == KindClaim || f.Kind == KindJoke
}

// Note is a short bracketed annotation for showing the fact to the model,
// e.g. "[unverified]" or "[state since Thu 14:20]"; "" for a plain fact.
func (f Fact) Note() string {
	var parts []string
	switch {
	case f.Kind == KindClaim || f.Kind == KindJoke:
		parts = append(parts, string(f.Kind)+", unverified")
	case f.Unverified():
		parts = append(parts, "unverified")
	}
	if f.Kind == KindState {
		parts = append(parts, "state since "+f.ValidFrom.Format("Mon 15:04"))
	}
	if !f.Valid() {
		r := f.Reason
		if r == "" {
			r = "outdated"
		}
		parts = append(parts, "no longer true since "+f.InvalidAt.Format("2006-01-02")+": "+r)
	}
	if len(parts) == 0 {
		return ""
	}
	return "[" + strings.Join(parts, "; ") + "]"
}

// String is the fact text with its note.
func (f Fact) String() string {
	if n := f.Note(); n != "" {
		return f.Text + " " + n
	}
	return f.Text
}

const factCols = `f.id, f.person, f.text, f.chat, f.said_by, f.tier, f.kind, f.valid_from, coalesce(f.invalid_at, 0), f.invalid_reason`

// order puts valid facts first, then higher tiers (J4).
var factOrder = `f.invalid_at IS NULL DESC, ` + rankSQL("f.tier") + ` DESC`

// queryFacts runs a query selecting factCols and appends up to limit facts
// not already in seen to out.
func (s *Store) queryFacts(ctx context.Context, limit int, seen map[int64]bool, out []Fact, q string, args ...any) ([]Fact, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() && len(out) < limit {
		var f Fact
		var from, inv int64
		if err := rows.Scan(&f.ID, &f.Person, &f.Text, &f.Chat, &f.SaidBy, &f.Tier, &f.Kind, &from, &inv, &f.Reason); err != nil {
			return out, err
		}
		if seen[f.ID] {
			continue
		}
		seen[f.ID] = true
		f.ValidFrom = time.Unix(from, 0)
		if inv != 0 {
			f.InvalidAt = time.Unix(inv, 0)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// migrateFacts adds the J1/J4 columns to a fact table from before them.
// Existing facts become tier legacy, valid from when they were saved, with
// their kind read from the label (J1: migrate and reclassify, delete nothing).
func migrateFacts(db *sql.DB) error {
	have := map[string]bool{}
	rows, err := db.Query(`SELECT name FROM pragma_table_info('person_facts')`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			rows.Close()
			return err
		}
		have[n] = true
	}
	rows.Close()
	added := false
	for _, c := range [][2]string{
		{"said_by", "TEXT NOT NULL DEFAULT ''"},
		{"tier", "TEXT NOT NULL DEFAULT 'legacy'"},
		{"kind", "TEXT NOT NULL DEFAULT 'fact'"},
		{"valid_from", "INTEGER NOT NULL DEFAULT 0"},
		{"invalid_at", "INTEGER"},
		{"invalid_reason", "TEXT NOT NULL DEFAULT ''"},
	} {
		if !have[c[0]] {
			if _, err := db.Exec(`ALTER TABLE person_facts ADD COLUMN ` + c[0] + ` ` + c[1]); err != nil {
				return err
			}
			added = true
		}
	}
	if added {
		if _, err := db.Exec(`UPDATE person_facts SET valid_from = ts WHERE valid_from = 0`); err != nil {
			return err
		}
		if err := reclassifyLegacy(db); err != nil {
			return err
		}
	}
	_, err = db.Exec(`CREATE INDEX IF NOT EXISTS person_facts_valid ON person_facts(person, invalid_at)`)
	return err
}

func reclassifyLegacy(db *sql.DB) error {
	rows, err := db.Query(`SELECT id, text FROM person_facts WHERE tier = 'legacy'`)
	if err != nil {
		return err
	}
	kinds := map[int64]Kind{}
	for rows.Next() {
		var id int64
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return err
		}
		if k := ClassifyKind(text, TierLegacy); k != KindFact {
			kinds[id] = k
		}
	}
	rows.Close()
	for id, k := range kinds {
		if _, err := db.Exec(`UPDATE person_facts SET kind = ? WHERE id = ?`, string(k), id); err != nil {
			return err
		}
	}
	return nil
}

// expireStates invalidates state facts older than StateTTL (J1).
func (s *Store) expireStates(ctx context.Context) error {
	ttl := int64(StateTTL / time.Second)
	_, err := s.db.ExecContext(ctx, `UPDATE person_facts SET invalid_at = valid_from + ?, invalid_reason = 'expired'
		WHERE kind = 'state' AND invalid_at IS NULL AND valid_from < ?`, ttl, s.now().Unix()-ttl)
	return err
}

// AddPersonFacts adds facts to a person's global profile (D3, D4) with their
// source (J1, J4). Re-saying a fact revives it if it was invalidated, refreshes
// a state, or raises its tier. A fact that nearly repeats a stored one
// overwrites it instead of piling up (J11).
func (s *Store) AddPersonFacts(ctx context.Context, key string, src Source, facts []string) error {
	if src.Tier == "" {
		src.Tier = TierInferred
	}
	for _, f := range facts {
		f = strings.TrimSpace(f)
		if f == "" {
			continue
		}
		if err := s.addFact(ctx, key, src, f); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) addFact(ctx context.Context, key string, src Source, text string) error {
	now := s.now().Unix()
	kind := string(ClassifyKind(text, src.Tier))
	if src.Kind != "" && kind != string(KindClaim) { // identity claims stay claims (J4)
		kind = string(src.Kind)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var exact int
	if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM person_facts WHERE person = ? AND text = ?`, key, text).Scan(&exact); err != nil {
		return err
	}
	if exact == 0 {
		// J11: overwrite near-duplicates with the same label.
		similar, err := similarFacts(ctx, tx, key, text)
		if err != nil {
			return err
		}
		for _, f := range similar {
			if f.Tier.rank() > src.Tier.rank() {
				return tx.Commit() // a more trusted version is already stored (J4)
			}
		}
		if len(similar) > 0 {
			keep := similar[0] // the most similar is overwritten in place
			for _, f := range similar[1:] {
				if _, err := tx.ExecContext(ctx, `DELETE FROM person_facts WHERE id = ?`, f.ID); err != nil {
					return err
				}
			}
			_, err := tx.ExecContext(ctx, `UPDATE person_facts SET text = ?, chat = ?, ts = ?, said_by = ?, tier = ?, kind = ?,
				valid_from = ?, invalid_at = NULL, invalid_reason = '' WHERE id = ?`,
				text, src.Chat, now, src.SaidBy, string(src.Tier), kind, now, keep.ID)
			if err != nil {
				return err
			}
			return tx.Commit()
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO person_facts(person, text, chat, ts, said_by, tier, kind, valid_from) VALUES (?,?,?,?,?,?,?,?)
		ON CONFLICT(person, text) DO UPDATE SET chat = excluded.chat, ts = excluded.ts, said_by = excluded.said_by,
			tier = excluded.tier, kind = excluded.kind, valid_from = excluded.valid_from, invalid_at = NULL, invalid_reason = ''
		WHERE person_facts.invalid_at IS NOT NULL OR person_facts.kind = 'state'
			OR `+rankSQL("excluded.tier")+` > `+rankSQL("person_facts.tier"),
		key, text, src.Chat, now, src.SaidBy, string(src.Tier), kind, now); err != nil {
		return err
	}
	return tx.Commit()
}

// Near-duplicates (J11): same label, and at least half the content words of
// the shorter fact shared, with at least 2 shared words. Aliases are never
// merged: every nickname is its own fact.
const (
	dupMinShared = 2
	dupMinRatio  = 0.5
)

func factLabel(text string) (label, body string) {
	l, b, ok := strings.Cut(text, ":")
	if !ok || len(l) > 20 {
		return "", text
	}
	return strings.ToLower(strings.TrimSpace(l)), b
}

// contentWords are a fact's distinctive words: lower-case, no stopwords or
// short words, with a plural "s" trimmed.
func contentWords(text string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}) {
		if len([]rune(w)) < 3 || stopwords[w] || genericWords[w] {
			continue
		}
		if len(w) > 4 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = w[:len(w)-1]
		}
		out[w] = true
	}
	return out
}

// genericWords say how, not what, so they don't count as shared content.
var genericWords = map[string]bool{"asks": true, "asked": true, "says": true, "said": true, "tells": true, "told": true,
	"calls": true, "called": true, "wants": true, "assistant": true, "bot": true, "chat": true, "group": true}

// nearDuplicate reports how similar two facts are (0 if not duplicates).
func nearDuplicate(a, b string) float64 {
	la, ba := factLabel(a)
	lb, bb := factLabel(b)
	if la != lb || la == "alias" {
		return 0
	}
	wa, wb := contentWords(ba), contentWords(bb)
	shared := 0
	for w := range wa {
		if wb[w] {
			shared++
		}
	}
	small := min(len(wa), len(wb))
	if shared < dupMinShared || small == 0 {
		return 0
	}
	r := float64(shared) / float64(small)
	if r < dupMinRatio {
		return 0
	}
	return r
}

// similarFacts returns the person's valid near-duplicates of text, most
// similar first.
func similarFacts(ctx context.Context, tx *sql.Tx, key, text string) ([]Fact, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, text, tier FROM person_facts WHERE person = ? AND invalid_at IS NULL`, key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type scored struct {
		f Fact
		r float64
	}
	var hits []scored
	for rows.Next() {
		var f Fact
		if err := rows.Scan(&f.ID, &f.Text, &f.Tier); err != nil {
			return nil, err
		}
		if r := nearDuplicate(text, f.Text); r > 0 {
			hits = append(hits, scored{f, r})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].r > hits[j].r })
	out := make([]Fact, len(hits))
	for i, h := range hits {
		out[i] = h.f
	}
	return out, nil
}

// ErrOutranked means the matching fact came from a higher tier than the
// one trying to invalidate it (J4).
var ErrOutranked = errors.New("that fact comes from a more trusted source")

// InvalidateFact marks a person's valid fact as no longer true, matching text
// case-insensitively (J1; D4's forget). It is kept as history. A source can
// only invalidate facts of its own tier or lower (J4).
func (s *Store) InvalidateFact(ctx context.Context, key, text string, by Tier, reason string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE person_facts SET invalid_at = ?, invalid_reason = ?
		WHERE person = ? AND lower(trim(text)) = lower(trim(?)) AND invalid_at IS NULL AND `+rankSQL("tier")+` <= ?`,
		s.now().Unix(), reason, key, text, by.rank())
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil
	}
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM person_facts
		WHERE person = ? AND lower(trim(text)) = lower(trim(?)) AND invalid_at IS NULL`, key, text).Scan(&n); err != nil {
		return false, err
	}
	if n > 0 {
		return false, ErrOutranked
	}
	return false, nil
}

// FactByID returns one fact, valid or not; ok is false if there is none.
func (s *Store) FactByID(ctx context.Context, id int64) (Fact, bool, error) {
	got, err := s.queryFacts(ctx, 1, map[int64]bool{}, nil, `SELECT `+factCols+` FROM person_facts f WHERE f.id = ?`, id)
	if err != nil || len(got) == 0 {
		return Fact{}, false, err
	}
	return got[0], true, nil
}

// InvalidateFactID is InvalidateFact for one fact by id (J3). It reports
// false if the fact is already invalid, and ErrOutranked if it comes from a
// higher tier than by (J4).
func (s *Store) InvalidateFactID(ctx context.Context, id int64, by Tier, reason string) (bool, error) {
	f, ok, err := s.FactByID(ctx, id)
	if err != nil || !ok || !f.Valid() {
		return false, err
	}
	if f.Tier.rank() > by.rank() {
		return false, ErrOutranked
	}
	_, err = s.db.ExecContext(ctx, `UPDATE person_facts SET invalid_at = ?, invalid_reason = ? WHERE id = ? AND invalid_at IS NULL`,
		s.now().Unix(), reason, id)
	return err == nil, err
}

// PersonFacts returns each key's valid facts, oldest first, with their notes
// (D7, J1).
func (s *Store) PersonFacts(ctx context.Context, keys []string) (map[string][]string, error) {
	if err := s.expireStates(ctx); err != nil {
		return nil, err
	}
	out := map[string][]string{}
	for _, k := range keys {
		facts, err := s.queryFacts(ctx, 1<<30, map[int64]bool{}, nil,
			`SELECT `+factCols+` FROM person_facts f WHERE f.person = ? AND f.invalid_at IS NULL ORDER BY f.id`, k)
		if err != nil {
			return nil, err
		}
		for _, f := range facts {
			out[k] = append(out[k], f.String())
		}
	}
	return out, nil
}

// FactCounts returns how many valid facts each key has (D7).
func (s *Store) FactCounts(ctx context.Context, keys []string) (map[string]int, error) {
	if err := s.expireStates(ctx); err != nil {
		return nil, err
	}
	out := map[string]int{}
	for _, k := range keys {
		var n int
		if err := s.db.QueryRowContext(ctx,
			`SELECT count(*) FROM person_facts WHERE person = ? AND invalid_at IS NULL`, k).Scan(&n); err != nil {
			return nil, err
		}
		out[k] = n
	}
	return out, nil
}

func validOnly(history bool) string {
	if history {
		return "1"
	}
	return "f.invalid_at IS NULL"
}

// SearchPersonFacts finds facts of the given people that match any word in
// query; an empty query lists their facts, newest first. Results are ranked
// by tier, then relevance (J4). Invalid facts are left out unless history is
// set (J1).
func (s *Store) SearchPersonFacts(ctx context.Context, keys []string, query string, limit int, history bool) ([]Fact, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	if err := s.expireStates(ctx); err != nil {
		return nil, err
	}
	in := strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",")
	args := make([]any, 0, len(keys)+2)
	var q string
	if strings.TrimSpace(query) == "" {
		q = `SELECT ` + factCols + ` FROM person_facts f WHERE f.person IN (` + in + `) AND ` + validOnly(history) + `
			ORDER BY ` + factOrder + `, f.id DESC LIMIT ?`
	} else {
		fq := ftsQuery(query)
		if fq == "" {
			return nil, nil
		}
		q = `SELECT ` + factCols + ` FROM person_facts_fts JOIN person_facts f ON f.id = person_facts_fts.rowid
			WHERE person_facts_fts MATCH ? AND f.person IN (` + in + `) AND ` + validOnly(history) + `
			ORDER BY ` + factOrder + `, bm25(person_facts_fts) LIMIT ?`
		args = append(args, fq)
	}
	for _, k := range keys {
		args = append(args, k)
	}
	args = append(args, limit)
	return s.queryFacts(ctx, limit, map[int64]bool{}, nil, q, args...)
}

// SearchAllFacts searches every known person's facts (D7): the newest facts
// of people whose name matches a word of the query, plus full-text matches on
// fact text. An empty query lists the newest facts overall. Invalid facts are
// left out unless history is set (J1).
func (s *Store) SearchAllFacts(ctx context.Context, query string, limit int, history bool) ([]Fact, error) {
	if err := s.expireStates(ctx); err != nil {
		return nil, err
	}
	var out []Fact
	seen := map[int64]bool{}
	var err error
	if strings.TrimSpace(query) == "" {
		return s.queryFacts(ctx, limit, seen, out, `SELECT `+factCols+` FROM person_facts f WHERE `+validOnly(history)+`
			ORDER BY f.id DESC LIMIT ?`, limit)
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
			// J2 b: rank their facts by the rest of the query ("what does
			// Omar draw" -> "draw"), falling back to their newest.
			rest := strings.TrimSpace(strings.Replace(strings.ToLower(query), w, " ", 1))
			if fq := ftsQuery(rest); fq != "" {
				if out, err = s.queryFacts(ctx, limit, seen, out, `SELECT `+factCols+` FROM person_facts_fts
					JOIN person_facts f ON f.id = person_facts_fts.rowid
					WHERE person_facts_fts MATCH ? AND f.person = ? AND `+validOnly(history)+`
					ORDER BY `+factOrder+`, bm25(person_facts_fts) LIMIT ?`, fq, p.Key, limit); err != nil {
					return nil, err
				}
			}
			if out, err = s.queryFacts(ctx, limit, seen, out, `SELECT `+factCols+` FROM person_facts f
				WHERE f.person = ? AND `+validOnly(history)+` ORDER BY `+factOrder+`, f.id DESC LIMIT ?`, p.Key, limit); err != nil {
				return nil, err
			}
		}
	}
	if fq := ftsQuery(query); fq != "" {
		out, err = s.queryFacts(ctx, limit, seen, out, `SELECT `+factCols+` FROM person_facts_fts
			JOIN person_facts f ON f.id = person_facts_fts.rowid
			WHERE person_facts_fts MATCH ? AND `+validOnly(history)+`
			ORDER BY `+factOrder+`, bm25(person_facts_fts) LIMIT ?`, fq, limit)
	}
	return out, err
}

// MergePerson moves a name-only or number-only profile's facts, with their
// history, onto another profile and deletes it (H3, J12). Only "name:" and
// "pn:" profiles can be merged away; LID profiles are real accounts.
func (s *Store) MergePerson(ctx context.Context, from, to string) (bool, error) {
	if !(strings.HasPrefix(from, "name:") || strings.HasPrefix(from, "pn:")) || from == to || to == "" {
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
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO person_facts
		(person, text, chat, ts, said_by, tier, kind, valid_from, invalid_at, invalid_reason)
		SELECT ?, text, chat, ts, said_by, tier, kind, valid_from, invalid_at, invalid_reason
		FROM person_facts WHERE person = ?`, to, from); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM person_facts WHERE person = ?`, from); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// MaxPreferences caps the standing preferences shown per prompt (K1).
const MaxPreferences = 5

// Preferences returns a person's standing preferences for every prompt (K1):
// valid owner-tier facts labelled "Prefers:" or "Rule:", newest first.
func (s *Store) Preferences(ctx context.Context, key string) ([]string, error) {
	if key == "" {
		return nil, nil
	}
	facts, err := s.queryFacts(ctx, MaxPreferences, map[int64]bool{}, nil,
		`SELECT `+factCols+` FROM person_facts f WHERE f.person = ? AND f.invalid_at IS NULL AND f.tier = ?
		 AND (lower(f.text) LIKE 'prefers:%' OR lower(f.text) LIKE 'rule:%') ORDER BY f.id DESC`, key, string(TierOwner))
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(facts))
	for _, f := range facts {
		out = append(out, f.Text)
	}
	return out, nil
}

// SetClock replaces the clock used for fact validity (tests and the golden
// set, which run at a fixed time).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// MaxStates caps the current states shown per person in every prompt (K4).
const MaxStates = 3

// RecentStates returns each person's newest valid state facts, newest first,
// at most MaxStates each, for every prompt (K4).
func (s *Store) RecentStates(ctx context.Context, keys []string) (map[string][]Fact, error) {
	if err := s.expireStates(ctx); err != nil {
		return nil, err
	}
	out := map[string][]Fact{}
	for _, k := range keys {
		if k == "" {
			continue
		}
		fs, err := s.queryFacts(ctx, MaxStates, map[int64]bool{}, nil,
			`SELECT `+factCols+` FROM person_facts f WHERE f.person = ? AND f.kind = 'state' AND f.invalid_at IS NULL
			 ORDER BY f.valid_from DESC, f.id DESC`, k)
		if err != nil {
			return nil, err
		}
		if len(fs) > 0 {
			out[k] = fs
		}
	}
	return out, nil
}
