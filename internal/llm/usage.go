package llm

import "context"

// J5: the caller can learn how many prompt tokens the first round of a
// request really used, to calibrate its own size estimate.

type usageKey struct{}

// WithPromptTokens returns a context whose request reports the first round's
// prompt tokens to f.
func WithPromptTokens(ctx context.Context, f func(tokens int)) context.Context {
	return context.WithValue(ctx, usageKey{}, f)
}

func reportPromptTokens(ctx context.Context, n int) {
	if f, ok := ctx.Value(usageKey{}).(func(int)); ok && n > 0 {
		f(n)
	}
}

// Stats is what one request cost (L4): filled in by clients that know, read
// by the caller after Reply returns. Not safe for concurrent requests.
type Stats struct {
	Cost         float64 // USD as reported by the provider
	Rounds       int     // model calls, including tool rounds
	PromptTokens int     // summed over rounds
	OutputTokens int
	WebSearches  int      // server-side web search calls
	CachedTokens int      // prompt tokens served from the provider's cache (P1)
	Providers    []string // who served the rounds, in order (P1)
}

type statsKey struct{}

// WithStats returns a context whose request adds its usage to s.
func WithStats(ctx context.Context, s *Stats) context.Context {
	return context.WithValue(ctx, statsKey{}, s)
}

func statsFrom(ctx context.Context) *Stats {
	s, _ := ctx.Value(statsKey{}).(*Stats)
	return s
}
