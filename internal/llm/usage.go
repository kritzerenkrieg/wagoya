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
