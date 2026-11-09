package router

import (
	"strings"
	"testing"
)

func TestToWhatsApp(t *testing.T) {
	for in, want := range map[string]string{
		"## Build\nUse **CMake**, *not* qmake.":    "*Build*\nUse *CMake*, _not_ qmake.",
		"* one\n  * two\n+ three":                  "- one\n  - two\n- three",
		"See [the docs](https://x.dev/a).":         "See the docs (https://x.dev/a).",
		"| Key | Does |\n|---|---|\n| p | place |": "Key · Does\np · place",
		"```\n**not bold** in code\n```":           "```\n**not bold** in code\n```",
		"a * b * c":                                "a * b * c",
		"`main.cpp:119`":                           "`main.cpp:119`",
	} {
		if got := ToWhatsApp(in); got != want {
			t.Errorf("ToWhatsApp(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTaskBody(t *testing.T) {
	long := strings.Repeat("line of the report\n", 400)
	b := TaskBody(TaskNote{ID: 5, Repo: "rvm_gui", Status: "done", Branch: "main", Result: "## Status\n**Works.**\n" + long})
	if !strings.HasPrefix(b, "*Task #5* (rvm_gui): done, branch main\n\n*Status*\n*Works.*") || !strings.HasSuffix(b, "…(cut; ask me for the full report)") || len([]rune(b)) > MaxReportChars+200 {
		t.Fatalf("done body = %q…", b[:200])
	}
	b = TaskBody(TaskNote{ID: 5, Repo: "r", Status: "needs_input", Result: "Looked.", Question: "SQLite or JSON?"})
	if !strings.HasSuffix(b, "Looked.\n\n*Question:* SQLite or JSON?") {
		t.Fatalf("question body = %q", b)
	}
	n := TaskNote{ID: 5, Repo: "r", Status: "running", Approval: "Run `cmake -S . -B build`"}
	if b = TaskBody(n); !n.Asks() || b != "Claude Code wants to do this (task #5, r):\nRun `cmake -S . -B build`\n\n"+ApprovalReply {
		t.Fatalf("live prompt = %q", b)
	}
	n = TaskNote{ID: 5, Repo: "r", Status: "needs_approval", Result: "Found it.", Approval: "- Run `make`"}
	if b = TaskBody(n); !n.Asks() || !strings.Contains(b, "Found it.") || !strings.Contains(b, "blocked from:\n- Run `make`") || !strings.Contains(b, "Reply *yes* to allow these") {
		t.Fatalf("blocked body = %q", b)
	}
	if (TaskNote{Status: "done"}).Asks() {
		t.Fatal("done asks")
	}
}
