package memtools

import (
	"context"
	"strings"
	"sync"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// Vectors embeds search queries for hybrid fact search (J2 c), caching the
// last few hundred query vectors.
type Vectors struct {
	E llm.Embedder

	mu    sync.Mutex
	cache map[string][]float32
}

const queryCacheSize = 256

// Vec returns the embedding of a query.
func (v *Vectors) Vec(ctx context.Context, q string) ([]float32, error) {
	key := strings.ToLower(strings.Join(strings.Fields(q), " "))
	v.mu.Lock()
	if hit, ok := v.cache[key]; ok {
		v.mu.Unlock()
		return hit, nil
	}
	v.mu.Unlock()
	vecs, err := v.E.Embed(ctx, []string{q})
	if err != nil {
		return nil, err
	}
	v.mu.Lock()
	if v.cache == nil || len(v.cache) >= queryCacheSize {
		v.cache = map[string][]float32{}
	}
	v.cache[key] = vecs[0]
	v.mu.Unlock()
	return vecs[0], nil
}

const (
	vecMaxHits = 8    // embedding hits kept per search
	vecSpread  = 0.10 // ...within this much cosine of the best hit
)

// hybrid merges embedding hits close to the best one with the FTS results,
// then orders them valid first, by tier, then by relevance (J2, J4).
func hybrid(vec []store.Scored, fts []store.Fact, limit int) []store.Fact {
	seen := map[int64]bool{}
	var out []store.Fact
	for i, h := range vec {
		if i >= vecMaxHits || h.Score < vec[0].Score-vecSpread {
			break
		}
		seen[h.ID] = true
		out = append(out, h.Fact)
	}
	for _, f := range fts {
		if !seen[f.ID] {
			seen[f.ID] = true
			out = append(out, f)
		}
	}
	if len(out) > limit {
		out = out[:limit]
	}
	store.Rank(out)
	return out
}

// Embed fills in vectors for facts that have none, up to batch at a time,
// and reports how many it embedded (J2 c: the bot's background embedder).
func Embed(ctx context.Context, db *store.Store, e llm.Embedder, batch int) (int, error) {
	todo, err := db.FactsWithoutVector(ctx, e.Model(), batch)
	if err != nil || len(todo) == 0 {
		return 0, err
	}
	texts := make([]string, len(todo))
	for i, u := range todo {
		texts[i] = u.Text
	}
	vecs, err := e.Embed(ctx, texts)
	if err != nil {
		return 0, err
	}
	for i, u := range todo {
		if err := db.SetFactVector(ctx, u.ID, e.Model(), vecs[i]); err != nil {
			return i, err
		}
	}
	return len(todo), nil
}
