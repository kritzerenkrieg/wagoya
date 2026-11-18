package store

import (
	"context"
	"encoding/binary"
	"math"
	"sort"
	"strings"
)

// Embedding vectors for hybrid fact search (J2 c). One vector per fact and
// model, as little-endian float32s; dropped when the fact is deleted or its
// text changes, and filled in again by the bot's background embedder.

const vectorsSchema = `
CREATE TABLE IF NOT EXISTS fact_vectors (
	fact_id INTEGER PRIMARY KEY,
	model TEXT NOT NULL,
	vec BLOB NOT NULL
);
CREATE TRIGGER IF NOT EXISTS fact_vectors_ad AFTER DELETE ON person_facts BEGIN
	DELETE FROM fact_vectors WHERE fact_id = old.id;
END;
CREATE TRIGGER IF NOT EXISTS fact_vectors_au AFTER UPDATE OF text ON person_facts WHEN old.text != new.text BEGIN
	DELETE FROM fact_vectors WHERE fact_id = old.id;
END;
`

func encodeVec(v []float32) []byte {
	b := make([]byte, 4*len(v))
	for i, x := range v {
		binary.LittleEndian.PutUint32(b[4*i:], math.Float32bits(x))
	}
	return b
}

func decodeVec(b []byte) []float32 {
	v := make([]float32, len(b)/4)
	for i := range v {
		v[i] = math.Float32frombits(binary.LittleEndian.Uint32(b[4*i:]))
	}
	return v
}

// EmbedText is what gets embedded for a fact: the owner's name and the text,
// so "what does Jack like" finds Jack's facts.
func EmbedText(name, text string) string {
	if name == "" {
		return text
	}
	return name + ": " + text
}

// Unembedded is a fact still without a vector for a model.
type Unembedded struct {
	ID   int64
	Text string // EmbedText of the fact
}

// FactsWithoutVector returns up to limit valid facts with no vector for model.
func (s *Store) FactsWithoutVector(ctx context.Context, model string, limit int) ([]Unembedded, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT f.id, coalesce(p.name, ''), f.text FROM person_facts f
		LEFT JOIN people p ON p.key = f.person
		LEFT JOIN fact_vectors v ON v.fact_id = f.id AND v.model = ?
		WHERE v.fact_id IS NULL AND f.invalid_at IS NULL ORDER BY f.id LIMIT ?`, model, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Unembedded
	for rows.Next() {
		var u Unembedded
		var name string
		if err := rows.Scan(&u.ID, &name, &u.Text); err != nil {
			return nil, err
		}
		u.Text = EmbedText(name, u.Text)
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetFactVector stores a fact's vector for model.
func (s *Store) SetFactVector(ctx context.Context, id int64, model string, v []float32) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO fact_vectors(fact_id, model, vec) VALUES (?,?,?)
		ON CONFLICT(fact_id) DO UPDATE SET model = excluded.model, vec = excluded.vec`, id, model, encodeVec(v))
	return err
}

// Scored is a fact with its similarity to a query.
type Scored struct {
	Fact
	Score float64
}

// VectorSearch ranks facts by cosine similarity to q (brute force: fine to
// ~100k facts). keys limits it to those people; nil searches everyone.
// Invalid facts are left out unless history is set (J1).
func (s *Store) VectorSearch(ctx context.Context, model string, q []float32, keys []string, limit int, history bool) ([]Scored, error) {
	if err := s.expireStates(ctx); err != nil {
		return nil, err
	}
	where := `v.model = ? AND ` + validOnly(history)
	args := []any{model}
	if keys != nil {
		if len(keys) == 0 {
			return nil, nil
		}
		where += ` AND f.person IN (` + strings.TrimSuffix(strings.Repeat("?,", len(keys)), ",") + `)`
		for _, k := range keys {
			args = append(args, k)
		}
	}
	rows, err := s.db.QueryContext(ctx, `SELECT v.fact_id, v.vec FROM fact_vectors v JOIN person_facts f ON f.id = v.fact_id WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	type hit struct {
		id    int64
		score float64
	}
	var hits []hit
	qn := norm(q)
	for rows.Next() {
		var id int64
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			rows.Close()
			return nil, err
		}
		v := decodeVec(b)
		if len(v) != len(q) || qn == 0 {
			continue
		}
		var dot float64
		for i := range v {
			dot += float64(v[i]) * float64(q[i])
		}
		if n := norm(v); n > 0 {
			hits = append(hits, hit{id, dot / (qn * n)})
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(hits, func(i, j int) bool { return hits[i].score > hits[j].score })
	if len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]Scored, 0, len(hits))
	for _, h := range hits {
		if f, ok, err := s.FactByID(ctx, h.id); err != nil {
			return nil, err
		} else if ok {
			out = append(out, Scored{f, h.score})
		}
	}
	return out, nil
}

func norm(v []float32) float64 {
	var n float64
	for _, x := range v {
		n += float64(x) * float64(x)
	}
	return math.Sqrt(n)
}

// Rank orders facts valid first, then by tier (J4), keeping the given order
// (relevance) within a tier.
func Rank(fs []Fact) {
	sort.SliceStable(fs, func(i, j int) bool {
		if fs[i].Valid() != fs[j].Valid() {
			return fs[i].Valid()
		}
		return fs[i].Tier.rank() > fs[j].Tier.rank()
	})
}
