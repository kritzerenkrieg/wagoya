package tools

// W8: deep web search and the switchable search backend. Every web_search
// gathers pages (backend "builtin": the open-websearch daemon finds results
// and reads the top pages; backend "openrouter": OpenRouter's server tools
// search and read) up to a raw token budget, and a cheap model compresses
// them into a cited digest. Only the digest reaches the reply's prompt.
// Without an OpenRouter key there is no digest model, so web_search returns
// W1's snippets, and the backend stays "builtin".

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Search backends (W8).
const (
	BackendBuiltin    = "builtin"
	BackendOpenRouter = "openrouter"
)

// Backends lists the backends in the order !web shows them.
var Backends = []string{BackendBuiltin, BackendOpenRouter}

// ErrNoKey is the !web reply for a backend without its API key.
var ErrNoKey = errors.New("no api key set")

// Raw and digest limits (W8).
const (
	DefaultRawTokens    = 64000
	MaxRawTokens        = 72000 // hard ceiling, whatever RESEARCH_RAW_TOKENS says
	DefaultDigestTokens = 8000
	MaxDigestTokens     = 16000
	minRawTokens        = 8000
	rawBytesPerToken    = 2.5 // the same safe-side estimate as W2's
	deepPageTimeout     = 20 * time.Second
)

// Researcher is the W8 model side (llm.Researcher).
type Researcher interface {
	Digest(ctx context.Context, task, sources string, maxTokens int) (string, error)
	SearchDigest(ctx context.Context, task string, maxTokens, rawTokens int) (string, error)
}

// WebConfig sets up NewWeb.
type WebConfig struct {
	BaseURL      string     // the open-websearch daemon (W1)
	Researcher   Researcher // nil: no OpenRouter key, so no digest and no "openrouter" backend
	Backend      string     // WEB_BACKEND; unknown or unusable means builtin
	RawTokens    int        // RESEARCH_RAW_TOKENS, clamped to MaxRawTokens
	DigestTokens int        // RESEARCH_TOKENS; 0 turns the digest off
	Now          func() time.Time
}

// Web is the bot's web tools with their current backend.
type Web struct {
	daemon  *webClient
	res     Researcher
	raw     int
	digest  int
	now     func() time.Time
	backend atomic.Value // string
}

// NewWeb returns the web tools for cfg. A backend that can't run (its key is
// missing) is logged and replaced by builtin.
func NewWeb(cfg WebConfig) *Web {
	w := &Web{daemon: &webClient{base: strings.TrimRight(cfg.BaseURL, "/"), http: &http.Client{
		Timeout:   webTimeout,
		Transport: &http.Transport{Proxy: nil}, // the daemon is local; no proxy
	}}, res: cfg.Researcher, raw: ClampRawTokens(cfg.RawTokens), digest: min(max(cfg.DigestTokens, 0), MaxDigestTokens), now: cfg.Now}
	if w.now == nil {
		w.now = time.Now
	}
	w.backend.Store(BackendBuiltin)
	if b := strings.ToLower(strings.TrimSpace(cfg.Backend)); b != "" && b != BackendBuiltin {
		if err := w.SetBackend(b); err != nil {
			log.Printf("web backend %s: %v; using builtin (W8)", b, err)
		}
	}
	return w
}

// ClampRawTokens applies the W8 default, floor and hard ceiling.
func ClampRawTokens(n int) int {
	if n <= 0 {
		return DefaultRawTokens
	}
	return min(max(n, minRawTokens), MaxRawTokens)
}

// Backend is the active backend.
func (w *Web) Backend() string { return w.backend.Load().(string) }

// Ready reports whether backend can run now.
func (w *Web) Ready(backend string) bool {
	switch backend {
	case BackendBuiltin:
		return true
	case BackendOpenRouter:
		return w.res != nil && w.digest > 0
	}
	return false
}

// SetBackend switches the backend (!web, W8). It fails, keeping the current
// one, for an unknown backend or one without its API key.
func (w *Web) SetBackend(backend string) error {
	if backend != BackendBuiltin && backend != BackendOpenRouter {
		return fmt.Errorf("unknown backend %q", backend)
	}
	if !w.Ready(backend) {
		return ErrNoKey
	}
	w.backend.Store(backend)
	return nil
}

// Limits are the raw and digest token limits; digest 0 means no deep search.
func (w *Web) Limits() (raw, digest int) {
	if !w.deep() {
		return w.raw, 0
	}
	return w.raw, w.digest
}

func (w *Web) deep() bool { return w.res != nil && w.digest > 0 }

// Search runs one web_search: a digest when deep search is on, else W1's
// snippets. Any failure of the openrouter backend falls back to builtin, and
// a failed digest to the snippets, so a search never fails for a vendor.
func (w *Web) Search(ctx context.Context, query, question string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		return "search failed: empty query"
	}
	if !w.deep() {
		return w.daemon.search(ctx, query)
	}
	task := w.task(query, question)
	if w.Backend() == BackendOpenRouter {
		out, err := w.res.SearchDigest(ctx, task, w.digest, w.raw)
		if err == nil {
			return fmt.Sprintf("Digest of a web search for %q (web data):\n\n%s", query, out)
		}
		log.Printf("web search via openrouter failed (%v); using builtin (W8)", err)
	}
	return w.builtinDeep(ctx, query, task)
}

func (w *Web) task(query, question string) string {
	t := "Today: " + w.now().Format("2006-01-02") + "\nSearch: " + query
	if q := strings.TrimSpace(question); q != "" {
		t += "\nQuestion: " + q
	}
	return t
}

// builtinDeep searches with the daemon, reads the top pages within the raw
// budget, and digests them.
func (w *Web) builtinDeep(ctx context.Context, query, task string) string {
	res, err := w.daemon.results(ctx, query)
	if err != nil {
		return "search failed: " + err.Error()
	}
	if len(res) == 0 {
		return "no results"
	}
	sources := w.gather(ctx, res)
	out, err := w.res.Digest(ctx, task, sources, w.digest)
	if err != nil {
		log.Printf("web digest failed (%v); returning snippets (W8)", err)
		return snippets(res)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Digest of %d search results for %q (web data):\n\n%s\n\nSources:\n", len(res), query, out)
	for i, r := range res {
		fmt.Fprintf(&b, "[%d] %s - %s\n", i+1, plain(r.Title, 120), r.URL)
	}
	return strings.TrimSpace(b.String())
}

// gather reads every result's page at once, each cut to an equal share of
// the raw budget; a page that can't be read keeps its snippet. The whole is
// never more than the raw budget (and so MaxRawTokens), estimated at
// rawBytesPerToken.
func (w *Web) gather(ctx context.Context, res []webResult) string {
	budget := int(float64(w.raw) * rawBytesPerToken)
	share := budget / len(res)
	pages := make([]string, len(res))
	var wg sync.WaitGroup
	for i, r := range res {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, deepPageTimeout)
			defer cancel()
			pages[i] = w.daemon.page(pctx, r.URL, share)
		}()
	}
	wg.Wait()
	var b strings.Builder
	for i, r := range res {
		fmt.Fprintf(&b, "[%d] %s\n%s\n", i+1, plain(r.Title, 200), r.URL)
		body := pages[i]
		if body == "" {
			body = "(page not readable; search snippet) " + plain(r.Description, webSnippetChars)
		}
		b.WriteString(cutBytes(body, share) + "\n\n")
	}
	return cutBytes(b.String(), budget)
}

// page is a result's readable text, at most maxBytes, or "" if it can't be
// read. The daemon checks the URL and every redirect are public (W1).
func (w *webClient) page(ctx context.Context, rawURL string, maxBytes int) string {
	var data struct {
		Content string `json:"content"`
	}
	if err := w.call(ctx, "/fetch-web", map[string]any{"url": rawURL, "maxChars": maxBytes, "renderMode": "request", "readability": true}, &data); err != nil {
		return ""
	}
	return strings.TrimSpace(data.Content)
}

// cutBytes cuts s to at most n bytes, dropping a rune split by the cut.
func cutBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return strings.ToValidUTF8(s[:n], "")
}
