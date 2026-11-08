package memtools

import (
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

func setup(t *testing.T) (*store.Store, func(req router.Line, spoke ...string) (*Turn, func(tool, input string) string)) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "m.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx, now := context.Background(), time.Now()
	for _, p := range [][2]string{{"john", "John"}, {"budi", "Budi"}, {"ana", "Ana"}} {
		db.UpsertPerson(ctx, p[0], p[0], "", p[1], now)
	}
	return db, func(req router.Line, spoke ...string) (*Turn, func(tool, input string) string) {
		hist := []router.Line{{SenderID: "ana", SenderName: "Ana", Text: "hi"}}
		handles := router.AssignHandles(hist, &req)
		tr := &Turn{DB: db, Chat: "g", Handles: handles, Requester: req, Spoke: map[string]bool{}}
		for _, h := range spoke {
			tr.Spoke[h] = true
		}
		tools := tr.Tools()
		return tr, func(tool, input string) string {
			for _, tl := range tools {
				if tl.Name == tool {
					return tl.Run(ctx, json.RawMessage(input))
				}
			}
			t.Fatalf("no tool %s", tool)
			return ""
		}
	}
}

var (
	john  = router.Line{SenderID: "john", SenderName: "John", Whitelisted: true}
	budi = router.Line{SenderID: "budi", SenderName: "Budi"}
)

func facts(db *store.Store, key string) []store.Fact {
	f, _ := db.SearchPersonFacts(context.Background(), []string{key}, "", 50, true)
	return f
}

func TestSaveFactChecksAndTiers(t *testing.T) {
	db, turn := setup(t)
	// Budi is P2, Ana P1 (she spoke).
	tr, call := turn(budi, "P2", "P1")
	for in, want := range map[string]string{
		`{"subject":"P2","content":"nurse"}`:                                 "short label",
		`{"subject":"P9","content":"Job: nurse"}`:                            "no participant",
		`{"subject":"P2","content":"Job: nurse","kind":"rumor"}`:             "kind must be",
		`{"subject":"","content":"Job: nurse"}`:                              "subject is empty",
		`{"subject":"P2","content":"Job: ` + strings.Repeat("x", 200) + `"}`: "too long",
	} {
		if got := call("save_fact", in); !strings.Contains(got, want) {
			t.Errorf("%s: got %q, want %q", in, got, want)
		}
	}
	if tr.Saved() != 0 {
		t.Fatal("rejected input was saved")
	}
	// About himself: self. About Ana, from Budi: hearsay. Ana said it herself: self.
	call("save_fact", `{"subject":"P2","content":"Job: nurse"}`)
	call("save_fact", `{"subject":"P1","content":"Job: dentist"}`)
	call("save_fact", `{"subject":"P1","content":"Likes: chess","source":"self"}`)
	call("save_fact", `{"subject":"Omar","content":"Likes: war history","kind":"claim"}`)
	if got := facts(db, "budi"); len(got) != 1 || got[0].Tier != store.TierSelf {
		t.Fatalf("budi: %+v", got)
	}
	tiers := map[string]store.Tier{}
	for _, f := range facts(db, "ana") {
		tiers[f.Text] = f.Tier
	}
	if tiers["Job: dentist"] != store.TierParticipant || tiers["Likes: chess"] != store.TierSelf {
		t.Fatalf("ana tiers: %v", tiers)
	}
	omar, _ := db.FindPeople(context.Background(), "Omar", 1)
	if len(omar) != 1 || facts(db, omar[0].Key)[0].Kind != store.KindClaim {
		t.Fatalf("omar: %v", omar)
	}
	// source self needs the subject to have spoken.
	_, call2 := turn(budi, "P2")
	if got := call2("save_fact", `{"subject":"P1","content":"Likes: tea","source":"self"}`); !strings.Contains(got, "source self") {
		t.Fatalf("self without speaking: %q", got)
	}
}

func TestInvalidateRights(t *testing.T) {
	db, turn := setup(t)
	ctx := context.Background()
	db.AddPersonFacts(ctx, "ana", store.Source{Chat: "g", SaidBy: "ana", Tier: store.TierSelf}, []string{"Job: nurse"})
	db.AddPersonFacts(ctx, "budi", store.Source{Chat: "g", SaidBy: "budi", Tier: store.TierSelf}, []string{"Job: driver"})
	_, call := turn(budi, "P2", "P1")
	res := call("search_facts", `{"person":"P1"}`)
	id := regexp.MustCompile(`fact (\d+) \| P1: Job: nurse`).FindStringSubmatch(res)
	if id == nil {
		t.Fatalf("search result has no id: %q", res)
	}
	if got := call("invalidate_fact", `{"id":`+id[1]+`,"reason":"quit"}`); !strings.Contains(got, "only owners") {
		t.Fatalf("budi retracted ana's fact: %q", got)
	}
	bid := regexp.MustCompile(`fact (\d+) \|`).FindStringSubmatch(call("search_facts", `{"person":"P2"}`))[1]
	if got := call("invalidate_fact", `{"id":`+bid+`,"reason":"new job"}`); got != "done" {
		t.Fatalf("budi could not retract his own fact: %q", got)
	}
	_, owner := turn(john, "P2")
	if got := owner("invalidate_fact", `{"id":`+id[1]+`,"reason":"asked to forget"}`); got != "done" {
		t.Fatalf("owner: %q", got)
	}
	if got := owner("invalidate_fact", `{"id":`+id[1]+`,"reason":"again"}`); got != "already outdated" {
		t.Fatalf("twice: %q", got)
	}
	if got := owner("invalidate_fact", `{"id":99999,"reason":"x"}`); !strings.Contains(got, "no fact") {
		t.Fatalf("missing id: %q", got)
	}
	if f := facts(db, "ana"); f[0].Valid() || !strings.Contains(f[0].Reason, "asked to forget") {
		t.Fatalf("not kept as history: %+v", f)
	}
}

func TestCuratorIsInferredAndMergesAliases(t *testing.T) {
	db, _ := setup(t)
	ctx := context.Background()
	db.PersonByName(ctx, "Yan", time.Now())
	db.AddPersonFacts(ctx, "name:yan", store.Source{Chat: "g", Tier: store.TierInferred}, []string{"Likes: futsal"})
	tr := &Turn{DB: db, Chat: "g", Handles: map[string]string{"P1": "ana"}}
	var save llm.Tool = tr.CuratorTools()[0]
	if _, ok := save.Schema["properties"].(map[string]any)["source"]; ok {
		t.Fatal("curator can pick a source")
	}
	save.Run(ctx, json.RawMessage(`{"subject":"P1","content":"Study: civil engineering","source":"self"}`))
	save.Run(ctx, json.RawMessage(`{"subject":"P1","content":"Alias: Yan"}`))
	got := facts(db, "ana")
	if len(got) != 3 || tr.Saved() != 2 {
		t.Fatalf("ana: %+v", got)
	}
	for _, f := range got {
		if f.Tier != store.TierInferred {
			t.Fatalf("curator fact not inferred: %+v", f)
		}
	}
}

func TestSaveMemory(t *testing.T) {
	db, turn := setup(t)
	_, call := turn(budi)
	if got := call("save_memory", `{"category":"gossip","content":"x y z"}`); !strings.Contains(got, "category must") {
		t.Fatalf("bad category: %q", got)
	}
	if got := call("save_memory", `{"category":"plan","content":"Futsal every Friday 8pm"}`); got != "saved" {
		t.Fatalf("save: %q", got)
	}
	if m, _ := db.SearchMemories(context.Background(), "g", "futsal", 5); len(m) != 1 {
		t.Fatalf("memory not stored: %v", m)
	}
}
