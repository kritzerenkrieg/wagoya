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

// TestOpenRouterLive calls OpenRouter for real: plain chat, search_facts and
// web search. Run with LLM_LIVE=1 and OPENROUTER_API_KEY set (G1-G4).
func TestOpenRouterLive(t *testing.T) {
	if os.Getenv("LLM_LIVE") == "" {
		t.Skip("set LLM_LIVE=1 to call OpenRouter")
	}
	config.LoadDotEnv("../../.env")
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		t.Skip("no OPENROUTER_API_KEY")
	}
	model := os.Getenv("OPENROUTER_TEST_MODEL")
	if model == "" {
		model = "deepseek/deepseek-v4.1-flash-1017"
	}
	c := NewOpenRouter(key, model, "low", true)
	sys := router.SystemPrompt("Rin", "You are Rin, John's quiet kuudere assistant who cares more than she admits.", false)
	search := func(_ context.Context, person, query string, _ bool) string {
		t.Logf("search_facts %q %q", person, query)
		return "P1: Origin: Lisbon, Portugal; lives in Porto.\nP1: Job: Nurse."
	}
	req := router.Line{SenderName: "John", SenderID: "lidA", Whitelisted: true}
	router.AssignHandles(nil, &req)
	people := []router.Person{{Handle: "P1", Name: "John", Whitelisted: true, FactCount: 2}}
	for _, q := range []string{"just got home, tired", "where am i from?", "search the web: newest Go release version?"} {
		p := router.BuildPrompt(router.Prompt{People: people, Requester: req, Text: q})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		start := time.Now()
		out, err := c.Reply(ctx, sys, p, nil, []Tool{FactSearchTool(search)})
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Q: %s\n(%.1fs) A: %s", q, time.Since(start).Seconds(), out)
		if strings.TrimSpace(out) == "" {
			t.Error("empty reply")
		}
	}
}
