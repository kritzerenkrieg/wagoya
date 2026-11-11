package main

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// backfillMinFacts is how many facts a person needs for a first profile
// written from them (V1).
const backfillMinFacts = 5

// backfillProfiles writes a first profile for everyone with enough stored
// facts and no profile yet (V1), one at a time. It runs once per start; once
// a person has a profile, the curator keeps it up to date.
func (b *bot) backfillProfiles() {
	if !b.curator.SupportsTools() {
		return // CLI mode: replies see every fact inline, and there is no read_profile
	}
	ctx := context.Background()
	keys, err := b.db.PeopleWithoutProfile(ctx, backfillMinFacts)
	if err != nil {
		log.Printf("profile backfill: %v", err)
		return
	}
	for _, k := range keys {
		if b.overBudget(ctx) { // M3
			log.Printf("profile backfill: over the daily budget, %d left for later", len(keys))
			return
		}
		b.backfillProfile(k)
	}
}

func (b *bot) backfillProfile(key string) {
	b.curMu.Lock() // not at the same time as a curator pass
	defer b.curMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if p, err := b.db.GetProfile(ctx, key); err != nil || p.Text != "" {
		return // a curator pass wrote one meanwhile
	}
	facts, err := b.db.PersonFacts(ctx, []string{key})
	if err != nil {
		log.Printf("profile backfill %s: %v", key, err)
		return
	}
	name, _ := b.db.PersonName(ctx, key)
	var st llm.Stats
	out, err := b.curator.Reply(llm.WithStats(ctx, &st), router.ProfileBackfillSystem, router.ProfileBackfillPrompt(name, facts[key]), nil, nil)
	b.account("profile backfill "+key, st)
	if err != nil {
		log.Printf("profile backfill %s: %v", key, err)
		return
	}
	text := router.ParseProfile(out, store.MaxProfileWords)
	if text == "" {
		log.Printf("profile backfill %s: no profile in the reply", key)
		return
	}
	if err := b.db.SetProfile(ctx, key, text, "first profile, from stored facts"); err != nil {
		log.Printf("profile backfill %s: %v", key, err)
		return
	}
	log.Printf("profile backfill: %s (%s) from %d facts, %d words", name, key, len(facts[key]), len(strings.Fields(text)))
}
