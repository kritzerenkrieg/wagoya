package llm

import (
	"log"
	"strings"
	"sync"
)

// M4: the Messages API reports tokens, not dollars, so the cost of a request
// is worked out here from a fixed price table (USD per million tokens). The
// table lives in code, like the M3 cap; a model missing from it is charged
// at the most expensive rates, so the cap errs on the side of stopping.

type price struct {
	in, out, cacheRead float64 // USD per MTok; 5-minute cache writes are 1.25x in
	// Prompts above longAbove input tokens are billed at the long card.
	longAbove                      int64
	longIn, longOut, longCacheRead float64
}

var prices = map[string]price{
	"claude-haiku-5-5":  {in: 0.10, out: 0.50, cacheRead: 0.01, longAbove: 100_000, longIn: 0.50, longOut: 2.50, longCacheRead: 0.05},
	"claude-haiku-4-5":  {in: 1, out: 5, cacheRead: 0.10},
	"claude-sonnet-5-5": {in: 2, out: 10, cacheRead: 0.10},
	"claude-sonnet-5":   {in: 2, out: 10, cacheRead: 0.20},
	"claude-sonnet-4-6": {in: 3, out: 15, cacheRead: 0.30},
	"claude-opus-5-5":   {in: 4, out: 20, cacheRead: 0.20},
	"claude-opus-5":     {in: 5, out: 25, cacheRead: 0.50},
	"claude-fable-5-2":  {in: 8, out: 40, cacheRead: 0.20},
	"claude-fable-5-1":  {in: 10, out: 50, cacheRead: 0.25},
}

// unknownPrice is the most expensive card in the table.
var unknownPrice = price{in: 10, out: 50, cacheRead: 1}

const webSearchUSD = 0.01 // $10 per 1,000 searches; web fetch costs only its tokens

var warnedModels sync.Map

// Usage is one Messages API response's token counts.
type Usage struct {
	Input, CacheWrite, CacheRead, Output, WebSearches int64
}

// AnthropicCost returns what one Messages API response cost in USD (M4).
func AnthropicCost(model string, u Usage) float64 {
	p, ok := prices[strings.TrimPrefix(model, "anthropic.")]
	if !ok {
		if _, seen := warnedModels.LoadOrStore(model, true); !seen {
			log.Printf("pricing: no prices for %q, charging the most expensive rates (M4)", model)
		}
		p = unknownPrice
	}
	in, out, cr := p.in, p.out, p.cacheRead
	if p.longAbove > 0 && u.Input+u.CacheWrite+u.CacheRead > p.longAbove {
		in, out, cr = p.longIn, p.longOut, p.longCacheRead
	}
	usd := (float64(u.Input)*in + float64(u.CacheWrite)*in*1.25 + float64(u.CacheRead)*cr + float64(u.Output)*out) / 1e6
	return usd + float64(u.WebSearches)*webSearchUSD
}
