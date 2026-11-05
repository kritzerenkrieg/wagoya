package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/router"
)

// TestSearchFactsLive checks with the real API that Claude calls search_facts
// and answers from the result. Run with LLM_LIVE=1.
func TestSearchFactsLive(t *testing.T) {
	if os.Getenv("LLM_LIVE") == "" {
		t.Skip("set LLM_LIVE=1 to call Claude")
	}
	config.LoadDotEnv("../../.env")
	key, model := os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("MODEL")
	if key == "" {
		t.Skip("no ANTHROPIC_API_KEY")
	}
	facts := []string{"Name: Alex Rivera, goes by Ken; Rin's owner.", "Origin: Lisbon, Portugal; lives in Porto.",
		"Job: IoT Systems Engineer, Acme Corp (since Dec 2025)."}
	var calls []string
	search := func(_ context.Context, person, query string, _ bool) string {
		calls = append(calls, person+"|"+query)
		var hits []string
		for _, f := range facts {
			for _, w := range strings.Fields(strings.ToLower(query)) {
				if len(w) > 2 && strings.Contains(strings.ToLower(f), w) {
					hits = append(hits, "P1: "+f)
					break
				}
			}
		}
		if query == "" || len(hits) == 0 { // mirrors main.go's fallback
			for _, f := range facts {
				hits = append(hits, "P1: "+f)
			}
		}
		if len(hits) == 0 {
			return "no matching facts"
		}
		return strings.Join(hits, "\n")
	}
	req := router.Line{SenderName: "Ken", SenderID: "lidA", Whitelisted: true}
	router.AssignHandles(nil, &req)
	p := router.BuildPrompt(router.Prompt{People: []router.Person{{Handle: "P1", Name: "Ken", Whitelisted: true, FactCount: 3}},
		Requester: req, Text: "where am i from and what do i do for work?"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	out, err := NewAPI(key, model, "low", false).Reply(ctx, router.SystemPrompt("Rin", "You are Rin, terse kuudere assistant.", false), p, nil, []Tool{FactSearchTool(search)})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("tool calls: %v\nreply: %s", calls, out)
	if len(calls) == 0 || !strings.Contains(out, "Lisbon") || !strings.Contains(strings.ToLower(out), "iot") {
		t.Fatal("expected a search and an answer from the facts")
	}
}
