package memtools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/router"
)

// V1: the curator rewrites a profile only with a reason; replies read it.
func TestProfileTools(t *testing.T) {
	db, turn := setup(t)
	ctx := context.Background()
	cur := &Turn{DB: db, Chat: "g", Handles: map[string]string{"P1": "ana", "P2": "john"}, Names: map[string]string{"P1": "Ana", "P2": "John"}}
	var upd = cur.CuratorTools()[1]
	if upd.Name != "update_profile" {
		t.Fatalf("curator tools: %s", upd.Name)
	}
	run := func(in string) string { return upd.Run(ctx, json.RawMessage(in)) }
	if out := run(`{"subject":"P1","profile":"Ana is an engineer.","reason":""}`); !strings.Contains(out, "reason is empty") {
		t.Fatalf("no reason: %s", out)
	}
	if out := run(`{"subject":"P9","profile":"x","reason":"y"}`); !strings.HasPrefix(out, "error") {
		t.Fatalf("unknown handle: %s", out)
	}
	if out := run(`{"subject":"Ana","profile":"x","reason":"y"}`); !strings.HasPrefix(out, "error") {
		t.Fatalf("name instead of handle: %s", out)
	}
	if out := run(`{"subject":"P1","profile":"` + strings.Repeat("word ", 401) + `","reason":"y"}`); !strings.Contains(out, "too long") {
		t.Fatalf("too long: %s", out)
	}
	if out := run(`{"subject":"P1","profile":"Ana is a civil engineer and P2's oldest friend; she teases him.","reason":"she said she started at a firm"}`); out != "updated" || cur.Saved() != 1 {
		t.Fatalf("update: %s", out)
	}
	p, _ := db.GetProfile(ctx, "ana")
	if p.Text != "Ana is a civil engineer and John's oldest friend; she teases him." || p.Reason != "she said she started at a firm" {
		t.Fatalf("stored: %+v", p)
	}

	// read_profile, by handle and by name from another chat.
	_, call := turn(router.Line{SenderID: "budi", SenderName: "Budi"})
	if out := call("read_profile", `{"person":"Ana"}`); !strings.Contains(out, "civil engineer") || !strings.Contains(out, "updated "+time.Now().Format("2006-01-02")) {
		t.Fatalf("read by name: %s", out)
	}
	if out := call("read_profile", `{"person":"John"}`); !strings.Contains(out, "no profile of John yet") {
		t.Fatalf("no profile: %s", out)
	}
	if out := call("read_profile", `{"person":"Nobody"}`); !strings.Contains(out, "no one known") {
		t.Fatalf("unknown: %s", out)
	}
}
