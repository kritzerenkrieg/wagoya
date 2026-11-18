package router

import (
	"sync"
)

// DefaultBytesPerToken is the starting estimate before any reply is measured
// (EstimateTokens uses it too).
const DefaultBytesPerToken = 3.0

// Ratio learns a model's real bytes per prompt token from the token counts
// replies report (J5 b): a running average, stored through save, used with a
// 10% safety margin.
type Ratio struct {
	mu   sync.Mutex
	avg  float64
	save func(float64)
}

const (
	ratioWeight = 0.2 // weight of a new observation
	ratioMargin = 0.9 // estimate 10% more tokens than the average says
	ratioMin    = 1.2 // clamp odd observations
	ratioMax    = 6.0
	ratioMinTok = 300 // too few tokens: fixed overhead dominates
)

// NewRatio starts from a stored average (0 = none yet); save persists updates.
func NewRatio(stored float64, save func(float64)) *Ratio {
	if stored < ratioMin || stored > ratioMax {
		stored = DefaultBytesPerToken
	}
	return &Ratio{avg: stored, save: save}
}

// BytesPerToken is the ratio to estimate with.
func (r *Ratio) BytesPerToken() float64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.avg * ratioMargin
}

// Observe records that bytes of prompt came to tokens.
func (r *Ratio) Observe(bytes, tokens int) {
	if tokens < ratioMinTok || bytes <= 0 {
		return
	}
	obs := min(max(float64(bytes)/float64(tokens), ratioMin), ratioMax)
	r.mu.Lock()
	r.avg = (1-ratioWeight)*r.avg + ratioWeight*obs
	avg := r.avg
	r.mu.Unlock()
	if r.save != nil {
		r.save(avg)
	}
}
