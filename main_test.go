package main

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/ratelimit"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
	"github.com/kritzerenkrieg/wagoya/internal/tools"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/types"
)

// L1: !key saves, lists and removes keys, swaps the owner tool, and never
// echoes the secret.
func TestKeyCommand(t *testing.T) {
	b := &bot{keysPath: filepath.Join(t.TempDir(), "api_keys.json"),
		newHTTPTool: func(k tools.Keys) llm.Tool {
			return llm.Tool{Name: "http_request", Description: strings.Join(k.Hosts(), ",")}
		}}
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
			t.Fatalf("!key %s = %q, want %q (and no secret)", s.args, got, s.want)
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

// L13: !provider switches to a ready provider and lists them otherwise.
func TestProviderCommand(t *testing.T) {
	cfg := &config.Config{Provider: "anthropic", AnthropicAPIKey: "a", OpenRouterKey: "k", DataDir: t.TempDir(),
		Ready: map[string]config.ProviderConf{
			"anthropic":  {Model: "claude-haiku-5-5"},
			"openrouter": {Model: "deepseek/deepseek-v4.1-flash-1017"},
		}}
	b := &bot{cfg: cfg}
	b.models.Store(newModels(cfg, cfg.Provider, b.effortFor(cfg.Provider), false))
	for _, tc := range []struct {
		args       []string
		want, kept string
	}{
		{nil, "Active provider: *anthropic*", "anthropic"},
		{[]string{"gemini"}, `Unknown provider "gemini".`, "anthropic"},
		{[]string{"openai"}, "openai isn't set up", "anthropic"},
		{[]string{"anthropic"}, "Already on anthropic.", "anthropic"},
		{[]string{"OpenRouter"}, "Switched to *openrouter*: deepseek/deepseek-v4.1-flash-1017", "openrouter"},
		{[]string{"openrouter", "x"}, "Active provider: *openrouter*", "openrouter"},
	} {
		got := b.providerCommand(tc.args)
		if !strings.Contains(got, tc.want) {
			t.Errorf("%v: %q, want %q", tc.args, got, tc.want)
		}
		if p := b.models.Load().provider; p != tc.kept {
			t.Errorf("%v: provider %s, want %s", tc.args, p, tc.kept)
		}
	}
	if !b.replyLLM().SupportsTools() || b.curatorLLM() == nil {
		t.Fatal("clients not swapped")
	}
}

// Y1: !reasoning sets the active provider's effort only, saves it, and each
// provider keeps its own across !provider switches and restarts; every
// provider starts at medium.
func TestReasoningCommand(t *testing.T) {
	ctx := context.Background()
	cfg := &config.Config{Provider: "anthropic", AnthropicAPIKey: "a", OpenRouterKey: "k", DataDir: t.TempDir(),
		Ready: map[string]config.ProviderConf{
			"anthropic":  {Model: "claude-haiku-5-5"},
			"openrouter": {Model: "deepseek/deepseek-v4.1-flash-1017"},
			"openai":     {Model: "m"},
		}}
	path := filepath.Join(cfg.DataDir, "bot.db")
	db, err := store.Open(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	start := func(db *store.Store) *bot { // what main does at startup
		b := &bot{cfg: cfg, db: db}
		b.loadEfforts(ctx)
		b.models.Store(newModels(cfg, cfg.Provider, b.effortFor(cfg.Provider), false))
		return b
	}
	b := start(db)
	effort := func() string { return b.models.Load().effort }
	for _, tc := range []struct {
		args       []string
		want, kept string
	}{
		{nil, "Reasoning on *anthropic*: medium\n", "medium"},
		{[]string{"max"}, `Unknown level "max".`, "medium"},
		{[]string{"low", "x"}, `Unknown level "low x".`, "medium"},
		{[]string{"medium"}, "Already at medium.", "medium"},
		{[]string{"HIGH"}, "Reasoning on *anthropic* is now high", "high"},
		{[]string{"none"}, "none (runs as low)", "none"},
	} {
		if got := b.reasoningCommand(ctx, tc.args); !strings.Contains(got, tc.want) {
			t.Errorf("%v: %q, want %q", tc.args, got, tc.want)
		}
		if effort() != tc.kept {
			t.Errorf("%v: effort %s, want %s", tc.args, effort(), tc.kept)
		}
	}
	// Switching provider uses that provider's own effort, not anthropic's.
	if got := b.providerCommand([]string{"openrouter"}); !strings.Contains(got, "effort medium") || effort() != "medium" {
		t.Fatalf("openrouter took another provider's effort: %q, %s", got, effort())
	}
	b.reasoningCommand(ctx, []string{"high"})
	if got := b.providerCommand([]string{"anthropic"}); !strings.Contains(got, "effort none (runs as low).") || effort() != "none" {
		t.Fatalf("anthropic lost its own effort: %q, %s", got, effort())
	}
	if list := b.providerCommand(nil); !strings.Contains(list, "openrouter: deepseek/deepseek-v4.1-flash-1017, effort high") {
		t.Errorf("list doesn't show openrouter's effort: %s", list)
	}
	// A restart keeps both saved levels; an unknown stored one is ignored.
	if err := db.SetSetting(ctx, effortKey("openai"), "turbo"); err != nil {
		t.Fatal(err)
	}
	db.Close()
	db2, err := store.Open(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	defer db2.Close()
	b = start(db2)
	if effort() != "none" || b.effortFor("openrouter") != "high" || b.effortFor("openai") != "medium" {
		t.Fatalf("after restart: anthropic %s, openrouter %s, openai %s", effort(), b.effortFor("openrouter"), b.effortFor("openai"))
	}
}

// D9: an over-long persona is recompressed whole, retried lower once, and
// rejected (old one kept) if it still doesn't fit; a short one is untouched.
func TestFitPersona(t *testing.T) {
	long := strings.Repeat("a", router.MaxPersonaRunes+10)
	short := "You are Bot."
	var limits []string
	ask := func(outs ...string) func(string, string) (string, error) {
		return func(system, prompt string) (string, error) {
			limits = append(limits, strings.SplitN(prompt, "\n", 2)[0])
			out := outs[0]
			outs = outs[1:]
			return out, nil
		}
	}
	if got, err := fitPersona(short, ask()); err != nil || got != short {
		t.Fatalf("short persona: %q %v", got, err)
	}
	if got, err := fitPersona(long, ask("<persona>"+short+"</persona>")); err != nil || got != short {
		t.Fatalf("recompressed: %q %v", got, err)
	}
	limits = nil
	if got, err := fitPersona(long, ask(long, short)); err != nil || got != short || len(limits) != 2 || !strings.Contains(limits[1], fmt.Sprint(router.MaxPersonaRunes-200)) {
		t.Fatalf("second try: %q %v %v", got, err, limits)
	}
	if _, err := fitPersona(long, ask(long, long)); err == nil {
		t.Fatal("still too long was accepted")
	}
	if _, err := fitPersona(long, func(string, string) (string, error) { return "", fmt.Errorf("down") }); err == nil {
		t.Fatal("model error was accepted")
	}
}

// F5: a group's last photo is attachable for five minutes, per chat.
func TestRecentImage(t *testing.T) {
	b := &bot{recentImg: map[string]recentImage{}}
	t0 := time.Date(2026, 11, 5, 14, 0, 0, 0, time.UTC)
	img := &waE2E.ImageMessage{}
	b.noteImage("g1", img, "Jane", t0)
	if r, ok := b.lastImage("g1", t0.Add(4*time.Minute)); !ok || r.img != img || r.from != "Jane" {
		t.Fatalf("recent photo not found: %+v %v", r, ok)
	}
	if _, ok := b.lastImage("g2", t0.Add(time.Minute)); ok {
		t.Fatal("photo leaked to another chat")
	}
	if _, ok := b.lastImage("g1", t0.Add(6*time.Minute)); ok {
		t.Fatal("stale photo attached")
	}
	b.noteImage("g2", img, "Jim", t0.Add(10*time.Minute)) // prunes g1
	if len(b.recentImg) != 1 {
		t.Fatalf("stale photos kept: %d", len(b.recentImg))
	}
}

// L15: !time shows the zone, saves a new offset (UTC when none is saved) and
// asks for a restart only when it changed.
func TestTimeCommand(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "bot.db"), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if z := loadZone(ctx, db); z.String() != "UTC" {
		t.Fatalf("default zone %s, want UTC", z)
	}
	old := time.Local
	defer func() { time.Local = old }()
	time.Local = time.UTC
	b := &bot{db: db}
	for _, tc := range []struct {
		args    []string
		want    string
		restart bool
	}{
		{nil, "Time zone: *UTC*", false},
		{[]string{"Asia/Jakarta"}, `Unknown offset "Asia/Jakarta".`, false},
		{[]string{"0"}, "Already on UTC.", false},
		{[]string{"+", "7"}, "Time zone set to *UTC+7*", true},
	} {
		got, restart := b.timeCommand(ctx, tc.args)
		if !strings.Contains(got, tc.want) || restart != tc.restart {
			t.Errorf("%v: %q (restart %v), want %q (restart %v)", tc.args, got, restart, tc.want, tc.restart)
		}
	}
	z := loadZone(ctx, db) // what the restart loads
	if _, off := time.Date(2026, 11, 20, 12, 0, 0, 0, z).Zone(); z.String() != "UTC+7" || off != 7*3600 {
		t.Fatalf("after restart: %s %d", z, off)
	}
}

// L15, U3: once shutdown starts the scheduler sends nothing new, a reminder
// it skips stays unmarked for the next start, and shutdown waits for a round
// in progress.
func TestSchedulerShutdown(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "bot.db"), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.AddTodo(ctx, store.Todo{Owner: "ann", Name: "Report", Due: time.Now().Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	todos, err := db.ListTodos(ctx, "ann", "", "", "", 10)
	if err != nil || len(todos) != 1 || todos[0].RemindAt().IsZero() {
		t.Fatalf("task: %v %v", todos, err)
	}
	now := todos[0].RemindAt().Add(time.Minute) // its reminder is due
	if due, err := db.TodoRemindersDue(ctx, now); err != nil || len(due) != 1 {
		t.Fatalf("reminder not due: %v %v", due, err)
	}
	quit, stop := context.WithCancel(ctx)
	b := &bot{db: db, quit: quit, schedBusy: make(chan struct{}, 1)}
	stop()
	b.runTaskReminders(now) // shutdown began: nothing is marked or sent
	if due, err := db.TodoRemindersDue(ctx, now); err != nil || len(due) != 1 {
		t.Fatalf("reminder lost on shutdown: %v %v", due, err)
	}
	b.schedBusy <- struct{}{} // a round in progress
	waited := make(chan struct{})
	go func() { b.waitScheduler(time.Minute, time.Second); close(waited) }()
	select {
	case <-waited:
		t.Fatal("shutdown didn't wait for the round")
	case <-time.After(50 * time.Millisecond):
	}
	<-b.schedBusy // the round ends
	select {
	case <-waited:
	case <-time.After(time.Second):
		t.Fatal("shutdown still waiting after the round ended")
	}
	done := make(chan struct{})
	go func() { b.runScheduler(); close(done) }()
	select {
	case <-done: // no new round after shutdown
	case <-time.After(time.Second):
		t.Fatal("scheduler kept running after shutdown")
	}
}

// U3, J10: a reminder or one-off is committed (marked, removed) only right
// before it goes out. One still pending when shutdown stops waiting, in the
// chat lock, the composition or the send queue, is cut off uncommitted and
// stays due for the next start; one committed is never undone.
func TestSchedulerShutdownCutsPendingSend(t *testing.T) {
	ctx := context.Background()
	db, err := store.Open(filepath.Join(t.TempDir(), "bot.db"), 64)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	const owner = "pn:15550001111"
	if _, err := db.AddTodo(ctx, store.Todo{Owner: owner, Name: "Report", Due: time.Now().Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	todos, err := db.ListTodos(ctx, owner, "", "", "", 10)
	if err != nil || len(todos) != 1 {
		t.Fatalf("task: %v %v", todos, err)
	}
	now := todos[0].RemindAt().Add(time.Minute)
	oneOff, err := db.AddJob(ctx, store.Job{Chat: "123@g.us", CreatedBy: "ann", Text: "stand-up", NextAt: now.Add(-time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	daily, err := db.AddJob(ctx, store.Job{Chat: "123@g.us", CreatedBy: "ann", Text: "water the plants", NextAt: now.Add(-time.Minute), Repeat: "daily"})
	if err != nil {
		t.Fatal(err)
	}
	newBot := func() (*bot, context.CancelFunc) {
		quit, stop := context.WithCancel(ctx)
		b := &bot{db: db, quit: quit, schedBusy: make(chan struct{}, 1), wlLID: map[string]bool{},
			cfg: &config.Config{WhitelistPN: map[string]bool{"15550001111": true}}}
		b.cut, b.cutSends = context.WithCancel(ctx)
		return b, stop
	}
	reminderDue := func() bool { due, _ := db.TodoRemindersDue(ctx, now); return len(due) == 1 }
	jobDue := func() bool { // both jobs still due, untouched
		jobs, _ := db.DueJobs(ctx, now)
		return len(jobs) == 2 && jobs[0].ID == oneOff && jobs[1].ID == daily
	}

	// Stuck before the commit (chat lock, composition, queue) past the wait.
	b, stop := newBot()
	pending := make(chan struct{}, 3)
	b.deliver = func(ctx context.Context, _ store.Job, _ types.JID, commit func() bool) {
		pending <- struct{}{}
		<-ctx.Done() // the round's context ends with cut, like a composition
	}
	b.schedBusy <- struct{}{}
	go func(b *bot) { b.runDue(now, 12*time.Hour, 30*time.Minute); b.runTaskReminders(now); <-b.schedBusy }(b)
	<-pending
	stop()
	b.waitScheduler(50*time.Millisecond, time.Second)
	if !b.cutOff() || !reminderDue() || !jobDue() {
		t.Fatalf("cut off before the commit: cut %v, reminder due %v, one-off due %v", b.cutOff(), reminderDue(), jobDue())
	}

	// The real send path: a queue wait cut by shutdown never commits.
	b, _ = newBot()
	b.pacer = ratelimit.NewPacer(time.Hour)
	_ = b.pacer.Wait(ctx) // the next slot is an hour away
	committed := false
	go b.cutSends()
	if id, ok := b.sendCommit(types.NewJID("15550001111", types.DefaultUserServer), "hi", nil, nil, func() bool { committed = true; return true }); id != "" || ok || committed {
		t.Fatalf("queued send cut off: id %q, committed %v %v", id, ok, committed)
	}

	// Committed, then stuck in the send itself past the wait: shutdown gives
	// up, and it stays committed (at most once: never sent twice).
	b, stop = newBot()
	stuck, worker := make(chan struct{}), make(chan struct{})
	defer func() { close(stuck); <-worker }() // the worker ends before the database closes
	b.deliver = func(_ context.Context, _ store.Job, _ types.JID, commit func() bool) {
		commit()
		<-stuck
	}
	b.schedBusy <- struct{}{}
	go func(b *bot) { b.runTaskReminders(now); <-b.schedBusy; close(worker) }(b)
	for reminderDue() {
		time.Sleep(time.Millisecond)
	}
	stop()
	start := time.Now()
	b.waitScheduler(50*time.Millisecond, 50*time.Millisecond)
	if d := time.Since(start); d > time.Second || reminderDue() {
		t.Fatalf("shutdown waited %s; reminder due again %v", d, reminderDue())
	}

	// Committed: the one-off removed, the daily one moved to its next run.
	b, _ = newBot()
	b.deliver = func(_ context.Context, _ store.Job, _ types.JID, commit func() bool) { commit() }
	b.runDue(now, 12*time.Hour, 30*time.Minute)
	jobs, err := db.DueJobs(ctx, now.Add(48*time.Hour))
	if err != nil || len(jobs) != 1 || jobs[0].ID != daily || !jobs[0].NextAt.After(now) {
		t.Fatalf("after sending: want only the daily job, moved past now: %+v %v", jobs, err)
	}
}
