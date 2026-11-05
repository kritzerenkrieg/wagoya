package llm

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/router"
)

// TestWebToolsLive makes real calls that need web_fetch and web_search.
// Run with LLM_LIVE=1.
func TestWebToolsLive(t *testing.T) {
	if os.Getenv("LLM_LIVE") == "" {
		t.Skip("set LLM_LIVE=1 to call Claude")
	}
	config.LoadDotEnv("../../.env")
	key, model := os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("MODEL")
	if key == "" {
		t.Skip("no ANTHROPIC_API_KEY")
	}
	c := NewAPI(key, model, "low", true)
	sys := router.SystemPrompt("Rin", "You are Rin, terse kuudere assistant.")
	for _, q := range []string{
		"read https://go.dev/doc/devel/release and tell me the newest Go version listed there",
		"search the web: what is the current price of Claude Sonnet 5.5 per million input tokens?",
		"compare deepseek vs claude api pricing per million tokens, check the web",
		"who is ebas alkafaroh, is he wanted by interpol?",
	} {
		req := router.Line{SenderName: "John", Whitelisted: true}
		p := router.BuildPrompt(router.Prompt{Requester: req, Text: q})
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		start := time.Now()
		out, err := c.Reply(ctx, sys, p, nil, nil)
		cancel()
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("Q: %s\n(%.1fs) A: %s", q, time.Since(start).Seconds(), out)
		if out == "" {
			t.Error("empty reply")
		}
	}
}
