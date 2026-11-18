package llm

import (
	"math"
	"testing"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

func TestAnthropicCost(t *testing.T) { // M4
	// 50k fresh input, 10k cache write, 20k cache read, 1k output, 2 searches on Haiku 5.5.
	u := Usage{Input: 50_000, CacheWrite: 10_000, CacheRead: 20_000, Output: 1_000, WebSearches: 2}
	want := (50_000*0.10+10_000*0.10*1.25+20_000*0.01+1_000*0.50)/1e6 + 0.02
	if got := AnthropicCost("claude-haiku-5-5", u); !near(got, want) {
		t.Fatalf("haiku: got %v want %v", got, want)
	}
	// Above 100k prompt tokens Haiku 5.5 switches to the long card.
	long := Usage{Input: 120_000, Output: 1_000}
	if got, want := AnthropicCost("claude-haiku-5-5", long), (120_000*0.50+1_000*2.50)/1e6; !near(got, want) {
		t.Fatalf("haiku long: got %v want %v", got, want)
	}
	// A Bedrock-style id uses the same card.
	if got, want := AnthropicCost("anthropic.claude-sonnet-5-5", Usage{Output: 1_000_000}), 10.0; !near(got, want) {
		t.Fatalf("sonnet: got %v want %v", got, want)
	}
	// A model missing from the table is charged at least the priciest card.
	unk := AnthropicCost("claude-unknown-9", Usage{Input: 1_000_000, Output: 1_000_000})
	for m := range prices {
		if p := AnthropicCost(m, Usage{Input: 1_000_000, Output: 1_000_000}); p > unk {
			t.Fatalf("unknown model charged %v, below %s at %v", unk, m, p)
		}
	}
}
