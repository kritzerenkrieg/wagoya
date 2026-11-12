package store

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Guessing who a name-only profile is (J12). A "name:" profile is made when
// someone is talked about before the bot has seen them; once a real account
// (LID or number) with a matching name shows up, the two are merged.

// NameProfile is a name-only profile and the chats its facts came from.
type NameProfile struct {
	Key   string
	Name  string
	Facts int
	Chats []string
}

// Account is a real (LID or number) profile that a name-only one may be.
type Account struct {
	Key  string
	Name string
	LID  string
	PN   string
}

// NameProfiles lists every name-only profile.
func (s *Store) NameProfiles(ctx context.Context) ([]NameProfile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.key, p.name, count(f.id), coalesce(group_concat(DISTINCT f.chat), '')
		FROM people p LEFT JOIN person_facts f ON f.person = p.key
		WHERE p.key LIKE 'name:%' GROUP BY p.key ORDER BY p.key`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameProfile
	for rows.Next() {
		var p NameProfile
		var chats string
		if err := rows.Scan(&p.Key, &p.Name, &p.Facts, &chats); err != nil {
			return nil, err
		}
		if chats != "" {
			p.Chats = strings.Split(chats, ",")
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// MergeCandidates returns the real accounts whose name or nickname matches
// name: a word of one equals, or starts, a word of the other (at least 3
// letters), e.g. "Richard" and "Richard Roe", or "Nikolas" and "Niko".
func (s *Store) MergeCandidates(ctx context.Context, name string) ([]Account, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT p.key, p.name, p.lid, p.pn,
		coalesce((SELECT group_concat(substr(text, 7), '|') FROM person_facts f
			WHERE f.person = p.key AND f.invalid_at IS NULL AND lower(f.text) LIKE 'alias:%'), '')
		FROM people p WHERE p.key NOT LIKE 'name:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		var aliases string
		if err := rows.Scan(&a.Key, &a.Name, &a.LID, &a.PN, &aliases); err != nil {
			return nil, err
		}
		for _, n := range append([]string{a.Name}, strings.Split(aliases, "|")...) {
			if namesMatch(name, n) {
				out = append(out, a)
				break
			}
		}
	}
	return out, rows.Err()
}

func namesMatch(a, b string) bool {
	wa, wb := strings.Fields(strings.ToLower(a)), strings.Fields(strings.ToLower(b))
	for _, x := range wa {
		for _, y := range wb {
			short, long := x, y
			if utf8.RuneCountInString(short) > utf8.RuneCountInString(long) {
				short, long = long, short
			}
			if short == long || (utf8.RuneCountInString(short) >= 3 && strings.HasPrefix(long, short)) {
				return true
			}
		}
	}
	return false
}

// PickMerge chooses the account a name-only profile belongs to, or ok=false
// when the guess isn't safe. Without facts any single match will do (nothing
// can be misfiled); with facts, the account must also be in a chat the facts
// came from (inChat), and exactly one may qualify.
func PickMerge(p NameProfile, cands []Account, inChat func(chat string, a Account) bool) (Account, bool) {
	if p.Facts == 0 {
		if len(cands) == 1 {
			return cands[0], true
		}
		return Account{}, false
	}
	var hit []Account
	for _, a := range cands {
		for _, c := range p.Chats {
			if inChat(c, a) {
				hit = append(hit, a)
				break
			}
		}
	}
	if len(hit) == 1 {
		return hit[0], true
	}
	return Account{}, false
}
