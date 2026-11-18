package router

import (
	"strings"
	"testing"
)

func TestBuildPromptFactCount(t *testing.T) {
	p := BuildPrompt(Prompt{People: []Person{{Handle: "P1", Name: "John", FactCount: 12}, {Handle: "P2", Name: "Ani"}}})
	if !strings.Contains(p, "P1 John (whitelisted=\"no\"): 12 stored facts, use search_facts") || !strings.Contains(p, "P2 Ani (whitelisted=\"no\"): no known facts") {
		t.Fatalf("fact counts not rendered:\n%s", p)
	}
}
