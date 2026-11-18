package llm

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"
)

// TestCLILive makes one real call through the claude CLI. Run with LLM_LIVE=1.
func TestCLILive(t *testing.T) {
	if os.Getenv("LLM_LIVE") == "" {
		t.Skip("set LLM_LIVE=1 to call Claude")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	c := NewCLI("claude", os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"), "claude-haiku-5-5", "low", t.TempDir(), false)
	out, err := c.Reply(ctx, "Reply with exactly the word PONG.", "ping", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "PONG") {
		t.Fatalf("unexpected reply %q", out)
	}
}
