package llm

import (
	"context"
	"testing"
)

// X1: replies set their own output limit; everything else keeps the default.
func TestMaxTokens(t *testing.T) {
	if got := maxTokens(context.Background()); got != DefaultMaxTokens {
		t.Errorf("default = %d", got)
	}
	if got := maxTokens(WithMaxTokens(context.Background(), ReplyMaxTokens)); got != ReplyMaxTokens {
		t.Errorf("reply = %d", got)
	}
	if got := maxTokens(WithMaxTokens(context.Background(), 0)); got != DefaultMaxTokens {
		t.Errorf("zero = %d", got)
	}
}
