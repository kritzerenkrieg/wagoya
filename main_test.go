package main

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/tools"
)

// L1: /key saves, lists and removes keys, swaps the owner tool, and never
// echoes the secret.
func TestKeyCommand(t *testing.T) {
	b := &bot{keysPath: filepath.Join(t.TempDir(), "api_keys.json"),
		newHTTPTool: func(k tools.Keys) llm.Tool { return llm.Tool{Name: "http_request", Description: strings.Join(k.Hosts(), ",")} }}
	const tok = "tok-secret-123456"
	steps := []struct{ args, want string }{
		{"", "No stored keys"},
		{"https://Tasks.Example.com/projects " + tok, "Saved a key for tasks.example.com (Bearer token)"},
		{"b.example.com X-Api-Key: " + tok, "Saved a key for b.example.com (header X-Api-Key)"},
		{"", "Stored keys for: b.example.com, tasks.example.com"},
		{"b.example.com remove", "Removed the key for b.example.com"},
		{"b.example.com remove", "No key stored"},
		{"localhost " + tok, "Usage"},
		{"tasks.example.com", "Usage"},
		{"tasks.example.com two words", "Usage"},
	}
	for _, s := range steps {
		got := b.keyCommand(strings.Fields(s.args))
		if !strings.Contains(got, s.want) || strings.Contains(got, tok) {
			t.Fatalf("/key %s = %q, want %q (and no secret)", s.args, got, s.want)
		}
	}
	k, err := tools.LoadKeys(b.keysPath)
	if err != nil || len(k) != 1 || k["tasks.example.com"]["Authorization"] != "Bearer "+tok {
		t.Fatalf("file: %v %v", k, err)
	}
	if d := b.httpOwner.Load().Description; d != "tasks.example.com" {
		t.Fatalf("owner tool not swapped: %q", d)
	}
}
