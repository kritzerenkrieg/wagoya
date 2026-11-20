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

// DefaultMaxTokens is each model call's output limit unless the request
// sets its own with WithMaxTokens.
const DefaultMaxTokens = 16000

// ReplyMaxTokens is a chat reply's output limit (X1): normal replies use
// under 4,500 tokens, thinking included; a runaway one (a prompt dump on a
// weak model) stops here instead of at 16,000.
const ReplyMaxTokens = 8000

type maxTokensKey struct{}

// WithMaxTokens returns a context whose model calls stop after n output tokens.
func WithMaxTokens(ctx context.Context, n int) context.Context {
	return context.WithValue(ctx, maxTokensKey{}, n)
}

func maxTokens(ctx context.Context) int {
	if n, ok := ctx.Value(maxTokensKey{}).(int); ok && n > 0 {
		return n
	}
	return DefaultMaxTokens
}
