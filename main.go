// Command wagoya is a WhatsApp number backed by an LLM (A1).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/mdp/qrterminal/v3"
	"go.mau.fi/whatsmeow"
	"go.mau.fi/whatsmeow/proto/waE2E"
	"go.mau.fi/whatsmeow/store/sqlstore"
	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"
	waLog "go.mau.fi/whatsmeow/util/log"
	"google.golang.org/protobuf/proto"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/memtools"
	"github.com/kritzerenkrieg/wagoya/internal/ratelimit"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
	"github.com/kritzerenkrieg/wagoya/internal/tools"
)

type bot struct {
	cfg    *config.Config
	wa     *whatsmeow.Client
	db     *store.Store
	claude llm.Client
	http   *llm.Tool  // I1: http_request, offered to everyone who can trigger the bot (I2)
	mcp    []llm.Tool // I1: MCP tools, offered only to whitelisted requesters (I2)
	chatMu sync.Map   // chat JID -> *sync.Mutex, one Claude call per chat at a time

	curator   llm.Client             // H1: background knowledge passes, no web tools
	curMu     sync.Mutex             // one curator pass at a time
	curIdleMu sync.Mutex             // guards curIdle
	curIdle   map[string]*time.Timer // per-chat idle pass timers

	pacer    *ratelimit.Pacer    // global outgoing message pacing (B18)
	denyCool *ratelimit.Cooldown // one denial per chat per DENY_COOLDOWN (B14)
	flood    *ratelimit.Flood    // M1: messages per person per minute

	budgetMu    sync.Mutex
	budgetNoted map[string]string // M3: chat -> day its over-budget notice went out

	schedOnce sync.Once // J7: one delivery loop

	ratio     *router.Ratio     // J5: learned bytes per prompt token of the model
	vectors   *memtools.Vectors // J2 c: hybrid search; nil means FTS only
	embedKick chan struct{}     // wakes the background embedder after a save

	wlMu  sync.RWMutex
	wlLID map[string]bool // configured LIDs plus LIDs resolved from phone numbers

	membersMu sync.Mutex
	members   map[string]memberCache // L5: group chat -> members, cached

	// I4, L1: stored API keys. httpOwner is http_request with the keys, for
	// whitelisted requesters; /key replaces it.
	keysMu      sync.Mutex
	keys        tools.Keys
	keysPath    string
	httpOwner   atomic.Pointer[llm.Tool]
	newHTTPTool func(tools.Keys) llm.Tool
}

func main() {
	if len(os.Args) > 1 && os.Args[1] == "task" { // O2: the laptop runner, over SSH
		if err := taskCommand(os.Args[2:], os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "task:", err)
			os.Exit(1)
		}
		return
	}
	if err := config.LoadDotEnv(".env"); err != nil {
		log.Fatalf("read .env: %v", err)
	}
	cfg, err := config.Load()
	if err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll(cfg.DataDir, 0o700); err != nil {
		log.Fatal(err)
	}
	if len(cfg.WhitelistPN)+len(cfg.WhitelistLID) == 0 {
		log.Print("WARNING: WHITELIST is empty; every DM is denied and the bot leaves every group (B15)")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := store.Open(filepath.Join(cfg.DataDir, "bot.db"), cfg.HistoryLimit)
	if err != nil {
		log.Fatalf("open bot db: %v", err)
	}
	defer db.Close()
	// K6: summaries written before the fix may hold per-prompt handles.
	if n, err := db.RewriteSummaries(ctx, func(t string) string { return router.StripHandles(t, nil) }); err != nil {
		log.Printf("scrub summaries: %v", err)
	} else if n > 0 {
		log.Printf("scrubbed handles from %d chat summaries", n)
	}

	claude := newClient(cfg, cfg.WebTools, true)
	curator := newClient(cfg, false, false) // H1: background passes never use the web

	waLogger := waLog.Stdout("WA", "WARN", false)
	container, err := sqlstore.New(ctx, "sqlite",
		"file:"+filepath.Join(cfg.DataDir, "session.db")+"?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", waLogger)
	if err != nil {
		log.Fatalf("open session store: %v", err)
	}
	device, err := container.GetFirstDevice(ctx)
	if err != nil {
		log.Fatalf("load device: %v", err)
	}

	var mcpTools *tools.MCP
	if cfg.APITools {
		mcpCfg, err := tools.ReadMCPConfig(cfg.MCPConfig)
		if err != nil {
			log.Printf("mcp config: %v", err)
		}
		mcpTools = tools.ConnectMCP(ctx, mcpCfg)
		defer mcpTools.Close()
	}
	b := &bot{cfg: cfg, db: db, claude: claude, curator: curator, curIdle: map[string]*time.Timer{}, wlLID: map[string]bool{},
		pacer: ratelimit.NewPacer(cfg.SendInterval), denyCool: ratelimit.NewCooldown(cfg.DenyCooldown),
		flood: ratelimit.NewFlood(floodLimit, floodWindow), budgetNoted: map[string]string{}}
	for l := range cfg.WhitelistLID {
		b.wlLID[l] = true
	}
	if cfg.APITools {
		hc := tools.NewHTTPClient()
		h := tools.HTTPTool(hc, nil)
		b.http, b.mcp = &h, mcpTools.Tools()
		// I4: stored API keys, used by http_request for whitelisted requesters only.
		b.keysPath = filepath.Join(cfg.DataDir, "api_keys.json")
		b.newHTTPTool = func(k tools.Keys) llm.Tool { return tools.HTTPTool(hc, k) }
		keys, err := tools.LoadKeys(b.keysPath)
		if err != nil {
			log.Printf("api keys: %v", err)
		}
		b.keys = keys
		hk := b.newHTTPTool(keys)
		b.httpOwner.Store(&hk)
		log.Printf("api tools: http_request for everyone, stored keys for %d hosts and %d MCP tools for whitelisted requesters", len(keys), len(b.mcp))
	}
	{ // J5: resume the learned ratio for this model
		key := "bytes_per_token:" + cfg.Model
		v, _ := db.Setting(context.Background(), key)
		stored, _ := strconv.ParseFloat(v, 64)
		b.ratio = router.NewRatio(stored, func(r float64) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := db.SetSetting(ctx, key, strconv.FormatFloat(r, 'f', 3, 64)); err != nil {
				log.Printf("save token ratio: %v", err)
			}
		})
	}
	if cfg.EmbedModel != "" {
		b.vectors = &memtools.Vectors{E: llm.NewOpenRouterEmbedder(cfg.OpenRouterKey, cfg.EmbedModel)}
		b.embedKick = make(chan struct{}, 1)
		go b.runEmbedder()
		log.Printf("hybrid search: embeddings with %s", cfg.EmbedModel)
	}
	b.wa = whatsmeow.NewClient(device, waLogger)
	b.wa.AddEventHandler(b.onEvent)

	if err := b.connect(ctx); err != nil {
		log.Fatal(err)
	}
	<-ctx.Done()
	log.Print("shutting down")
	b.wa.Disconnect()
}

// connect links the device on first run (B10), then connects.
func (b *bot) connect(ctx context.Context) error {
	if b.wa.Store.ID != nil {
		return b.wa.Connect()
	}
	qr, err := b.wa.GetQRChannel(ctx)
	if err != nil {
		return err
	}
	if err := b.wa.Connect(); err != nil {
		return err
	}
	if b.cfg.PairNumber != "" {
		code, err := b.wa.PairPhone(ctx, b.cfg.PairNumber, true, whatsmeow.PairClientChrome, "Chrome (Linux)")
		if err != nil {
			return fmt.Errorf("pair phone: %w", err)
		}
		log.Printf("PAIRING CODE: %s  (WhatsApp > Linked devices > Link with phone number)", code)
	}
	go func() {
		for item := range qr {
			switch item.Event {
			case "code":
				if b.cfg.PairNumber == "" {
					log.Print("Scan this QR in WhatsApp > Linked devices:")
					qrterminal.GenerateHalfBlock(item.Code, qrterminal.L, os.Stdout)
				}
			case "success":
				log.Print("linked")
			default:
				log.Printf("link: %s", item.Event)
			}
		}
	}()
	return nil
}

func (b *bot) onEvent(evt any) {
	switch e := evt.(type) {
	case *events.Message:
		go b.onMessage(e)
	case *events.Connected:
		log.Printf("connected as %s (LID %s)", b.wa.Store.ID, b.wa.Store.GetLID())
		go func() {
			b.resolveWhitelist()
			b.auditGroups()
			b.guessMerges()
			b.schedOnce.Do(func() { go b.runScheduler(); go b.runTasks(); go b.backfillProfiles() }) // V1: backfill
		}()
	case *events.JoinedGroup:
		go b.onJoined(e)
	case *events.GroupInfo:
		go b.onGroupChange(e)
	case *events.LoggedOut:
		log.Print("LOGGED OUT by WhatsApp; delete data/session.db and relink")
	}
}

// identify returns the LID and phone-number user parts of a sender, using the
// JID, its alternate and whatsmeow's LID<->PN mapping store (B9). Identity
// never comes from names or message text.
func (b *bot) identify(ctx context.Context, jid, alt types.JID) (lid, pn string) {
	for _, j := range []types.JID{jid, alt} {
		switch j.Server {
		case types.HiddenUserServer:
			lid = j.User
		case types.DefaultUserServer:
			pn = j.User
		}
	}
	if lid == "" && pn != "" {
		if l, err := b.wa.Store.LIDs.GetLIDForPN(ctx, types.NewJID(pn, types.DefaultUserServer)); err == nil && !l.IsEmpty() {
			lid = l.User
		}
	}
	if pn == "" && lid != "" {
		if p, err := b.wa.Store.LIDs.GetPNForLID(ctx, types.NewJID(lid, types.HiddenUserServer)); err == nil && !p.IsEmpty() {
			pn = p.User
		}
	}
	return lid, pn
}

// allowed reports whether a sender is whitelisted by LID or phone number.
func (b *bot) allowed(lid, pn string) bool {
	b.wlMu.RLock()
	defer b.wlMu.RUnlock()
	return (lid != "" && b.wlLID[lid]) || (pn != "" && b.cfg.WhitelistPN[pn])
}

func (b *bot) whitelisted(ctx context.Context, jid, alt types.JID) bool {
	return b.allowed(b.identify(ctx, jid, alt))
}

// resolveWhitelist looks up the LID of every whitelisted phone number, so the
// owner only needs to know their own number.
func (b *bot) resolveWhitelist() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var ask []types.JID
	for pn := range b.cfg.WhitelistPN {
		j := types.NewJID(pn, types.DefaultUserServer)
		if l, err := b.wa.Store.LIDs.GetLIDForPN(ctx, j); err == nil && !l.IsEmpty() {
			b.addLID(pn, l.User)
			continue
		}
		ask = append(ask, j)
	}
	if len(ask) == 0 {
		return
	}
	info, err := b.wa.GetUserInfo(ctx, ask)
	if err != nil {
		log.Printf("resolve whitelist: %v (phone-number matching still works)", err)
		return
	}
	for _, j := range ask {
		if u, ok := info[j]; ok && !u.LID.IsEmpty() {
			b.addLID(j.User, u.LID.User)
		} else {
			log.Printf("whitelist: %s has no LID (not on WhatsApp?)", j.User)
		}
	}
}

func (b *bot) addLID(pn, lid string) {
	b.wlMu.Lock()
	b.wlLID[lid] = true
	b.wlMu.Unlock()
	log.Printf("whitelist: %s -> LID %s", pn, lid)
}

func (b *bot) lock(chat string) func() {
	m, _ := b.chatMu.LoadOrStore(chat, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}

func messageText(m *waE2E.Message) (text string, img *waE2E.ImageMessage) {
	if m == nil {
		return "", nil
	}
	switch {
	case m.GetConversation() != "":
		return m.GetConversation(), nil
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetText(), nil
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetCaption(), m.GetImageMessage()
	case m.GetEphemeralMessage() != nil:
		return messageText(m.GetEphemeralMessage().GetMessage())
	case m.GetViewOnceMessage() != nil:
		return messageText(m.GetViewOnceMessage().GetMessage())
	}
	return "", nil
}

// contextInfo returns the reply/quote metadata of a message, if any.
func contextInfo(m *waE2E.Message) *waE2E.ContextInfo {
	switch {
	case m == nil:
		return nil
	case m.GetExtendedTextMessage() != nil:
		return m.GetExtendedTextMessage().GetContextInfo()
	case m.GetImageMessage() != nil:
		return m.GetImageMessage().GetContextInfo()
	case m.GetEphemeralMessage() != nil:
		return contextInfo(m.GetEphemeralMessage().GetMessage())
	case m.GetViewOnceMessage() != nil:
		return contextInfo(m.GetViewOnceMessage().GetMessage())
	}
	return nil
}

// addressed reports whether a group message is meant for the bot: it
// @mentions the bot (a real WhatsApp mention) or replies to one of the bot's
// messages (B1). Mention tokens like "@6289..." become "@<BOT_NAME>".
func (b *bot) addressed(m *waE2E.Message, text string) (bool, string) {
	ci := contextInfo(m)
	if ci == nil {
		return false, text
	}
	hit := false
	for _, s := range ci.GetMentionedJID() {
		if j, err := types.ParseJID(s); err == nil && b.isMe(j) {
			hit = true
			text = strings.ReplaceAll(text, "@"+j.User, "@"+b.cfg.BotName)
		}
	}
	if ci.GetQuotedMessage() != nil {
		if j, err := types.ParseJID(ci.GetParticipant()); err == nil && b.isMe(j) {
			hit = true
		}
	}
	return hit, text
}

// quoted resolves the message being replied to: who wrote it (by WhatsApp
// ID, B9), its text, and its image if it had one (F1).
func (b *bot) quoted(ctx context.Context, m *waE2E.Message) (name, text string, img *waE2E.ImageMessage) {
	ci := contextInfo(m)
	if ci == nil || ci.GetQuotedMessage() == nil {
		return "", "", nil
	}
	text, img = messageText(ci.GetQuotedMessage())
	if img != nil {
		text = strings.TrimSpace("[image] " + text)
	}
	if j, err := types.ParseJID(ci.GetParticipant()); err == nil && !j.IsEmpty() {
		if b.isMe(j) {
			name = b.cfg.BotName
		} else {
			lid, pn := b.identify(ctx, j, types.EmptyJID)
			name, _ = b.db.PersonName(ctx, personKey(lid, pn))
		}
	}
	return name, text, img
}

func (b *bot) onMessage(e *events.Message) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	chat := e.Info.Chat
	if chat.Server == types.BroadcastServer || chat.Server == types.NewsletterServer {
		return
	}
	if rm := e.Message.GetReactionMessage(); rm != nil {
		b.onReaction(ctx, e, rm)
		return
	}
	text, img := messageText(e.Message)
	if text == "" && img == nil {
		return // B8: other media ignored
	}
	isGroup := chat.Server == types.GroupServer
	lid, pn := b.identify(ctx, e.Info.Sender, e.Info.SenderAlt)
	wl := b.allowed(lid, pn)
	key := personKey(lid, pn)
	if !wl && !e.Info.IsFromMe && b.isMuted(ctx, key) {
		return // M2: muted or timed out (M1): not answered, stored or curated
	}

	// L1, L2: owner commands, handled here and never stored or shown to the
	// model, so a token sent with /key stays out of history and prompts.
	cmdText := text
	if isGroup {
		_, cmdText = b.addressed(e.Message, text)
	}
	if cmd, ok := router.ParseCommand(cmdText); ok && img == nil {
		b.command(ctx, e, cmd, isGroup, wl)
		return
	}

	addressed := false
	if isGroup {
		addressed, text = b.addressed(e.Message, text)
	}
	d := router.Decide(router.Incoming{IsGroup: isGroup, FromMe: e.Info.IsFromMe, Whitelisted: wl, Addressed: addressed, Text: text}, b.cfg.Trigger)
	switch d.Action {
	case router.Ignore:
		return
	case router.Deny:
		if !b.denyCool.Allow(chat.String(), time.Now()) {
			log.Printf("denied DM from %s (LID %q, number %q), cooldown: no reply", e.Info.Sender, lid, pn)
			return
		}
		log.Printf("denied DM from %s (LID %q, number %q)", e.Info.Sender, lid, pn)
		b.send(ctx, chat, router.DenyText, nil)
		return
	}
	if !wl && !e.Info.IsFromMe && key != "" {
		floodText := text
		if img != nil {
			floodText = strings.TrimSpace("[image] " + text)
		}
		if b.floodCheck(ctx, e, key, floodText) {
			return // M1: timed out just now
		}
		if d.Action == router.Reply && b.overBudget(ctx) {
			b.budgetNotice(ctx, chat, e) // M3: store it, don't answer
			d.Action = router.Store
		}
	}

	stored := text
	if img != nil {
		stored = strings.TrimSpace("[image] " + text)
	}
	qName, qText, qImg := b.quoted(ctx, e.Message)
	stored = router.WithQuote(stored, qName, qText, 300)
	prompt := router.WithQuote(d.Prompt, qName, qText, 4000)
	line := router.Line{Time: e.Info.Timestamp, SenderName: e.Info.PushName, SenderID: key, Whitelisted: wl, Text: stored, ID: e.Info.ID}
	line.ToBot, line.To = b.addressees(ctx, e.Message, isGroup, qName, key) // K7
	chatKey := chat.String()
	if key != "" { // D3: remember everyone the bot hears, by LID/number
		if err := b.db.UpsertPerson(ctx, key, lid, pn, e.Info.PushName, e.Info.Timestamp); err != nil {
			log.Printf("upsert person: %v", err)
		}
	}

	if d.Action == router.Store {
		if err := b.db.AddMessage(ctx, chatKey, line); err != nil {
			log.Printf("store message: %v", err)
		}
		b.noteMessage(chatKey)
		return
	}

	unlock := b.lock(chatKey)
	defer unlock()
	if !wl && !e.Info.IsFromMe && b.isMuted(ctx, key) {
		return // M2: timed out or muted while waiting for the chat
	}

	history, err := b.db.History(ctx, chatKey) // window before this message
	if err != nil {
		log.Printf("history: %v", err)
	}
	if err := b.db.AddMessage(ctx, chatKey, line); err != nil {
		log.Printf("store message: %v", err)
	}
	b.noteMessage(chatKey)
	if wl && key != "" && img == nil && b.handleApproval(ctx, chat, e, key, text, qText) {
		return // R4: an owner's yes or no to Claude Code, read by the server
	}
	summary, err := b.db.Summary(ctx, chatKey) // J8
	if err != nil {
		log.Printf("summary: %v", err)
	}
	var reminders []router.Reminder // N3
	if jobs, err := b.db.ListJobs(ctx, chatKey, ""); err != nil {
		log.Printf("reminders: %v", err)
	} else {
		for _, j := range jobs {
			_, text, _ := router.ExpandMentions(j.Text, b.nameOf(ctx))
			reminders = append(reminders, router.Reminder{At: j.NextAt, Text: text, Repeat: j.Repeat})
		}
	}
	mems, err := b.db.SearchMemories(ctx, chatKey, prompt, b.cfg.MemoryResults)
	if err != nil {
		log.Printf("memories: %v", err)
	}

	var image *llm.Image
	if img == nil && qImg != nil && d.Action == router.Reply {
		img = qImg // F1: "@Rin what's this" replying to an image
	}
	if img != nil {
		data, err := b.wa.Download(ctx, img)
		if err != nil {
			log.Printf("download image: %v", err)
		} else {
			image = &llm.Image{MediaType: img.GetMimetype(), Data: data}
		}
	}
	if prompt == "" && image == nil {
		return
	}

	persona, err := b.db.Persona(ctx)
	if err != nil {
		log.Printf("persona: %v", err)
	}
	system := router.SystemPrompt(b.cfg.BotName, persona, b.claude.SupportsTools())
	handles := router.AssignHandles(history, &line)
	var members map[string]member
	if isGroup { // L5: members who haven't spoken get handles too
		var keys []string
		keys, members = b.groupMembers(ctx, chat)
		router.AddHandles(handles, keys)
	}
	people := b.people(ctx, handles, history, line, members)

	_ = b.wa.SendChatPresence(ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	// J3: memory is written with checked tools; tags only in CLI mode.
	names := map[string]string{}
	for _, p := range people {
		names[p.Handle] = p.Name
	}
	turn := &memtools.Turn{DB: b.db, Chat: chatKey, Handles: handles, Names: names, Requester: line, Spoke: map[string]bool{line.Handle: true},
		// J10: groups without an owner are left (E1), so every group has one.
		ChatHasOwner: isGroup || wl, ResolveChat: b.chatResolver(lid, pn), Vectors: b.vectors, AfterSave: b.kickEmbedder}
	for _, l := range history {
		turn.Spoke[l.Handle] = true
	}
	toolset := append(append(turn.Tools(), turn.ScheduleTools()...), turn.BacklogTools()...) // U1
	switch {
	case line.Whitelisted && b.httpOwner.Load() != nil: // I4: stored keys are owner-only
		toolset = append(toolset, *b.httpOwner.Load())
	case b.http != nil:
		toolset = append(toolset, *b.http)
	}
	if line.Whitelisted { // I2: MCP tools are owner-only; O1: so is Claude Code
		toolset = append(toolset, b.mcp...)
		toolset = append(toolset, turn.ClaudeTools()...)
	}
	// J5: tool definitions count in the fixed part; the ratio is learned
	// from what each text-only reply's first round really cost.
	toolBytes := 0
	for _, t := range toolset {
		j, _ := json.Marshal(t.Schema)
		toolBytes += len(t.Name) + len(t.Description) + len(j)
	}
	full := router.FitPromptWith(system, router.Prompt{
		Memories: mems, Summary: summary, Reminders: reminders, People: people, History: history, Requester: line, Text: prompt, HasImage: image != nil, Now: time.Now(), Group: isGroup,
	}, b.cfg.ContextTokens, b.ratio.BytesPerToken(), toolBytes)
	rctx := ctx
	if image == nil {
		sent := len(system) + len(full) + toolBytes
		rctx = llm.WithPromptTokens(ctx, func(n int) { b.ratio.Observe(sent, n) })
	}
	var st llm.Stats // L4: what this reply cost
	reply, err := b.claude.Reply(llm.WithStats(rctx, &st), system, full, image, toolset)
	b.account("reply "+chatKey, st)
	_ = b.wa.SendChatPresence(ctx, chat, types.ChatPresencePaused, types.ChatPresenceMediaText)
	if err != nil {
		log.Printf("claude: %v", err)
		b.send(ctx, chat, "Sorry, I couldn't reach Claude right now.", e)
		return
	}
	parsed := router.ParseReply(reply)
	b.applyTags(ctx, chatKey, line, handles, parsed, !b.claude.SupportsTools())
	reply = parsed.Text
	if turn.Queued() { // O5: the marker is the server's, never left to the model
		if reply == "" {
			reply = "On it."
		}
		reply = memtools.WithMarker(memtools.MarkInitiated, reply)
	}
	if reply == "" {
		return
	}
	// L3: "@P2" becomes a real mention; history keeps "@Name".
	out, stored, mentions := router.ResolveMentions(reply, handles, names)
	sentID := b.sendMentions(ctx, chat, out, e, mentionJIDs(mentions))
	reply = stored
	// K7: a reply is addressed to its requester (it quotes them) and whoever it tags.
	to := append([]string{line.SenderName}, b.mentionNames(ctx, mentions, line.SenderID)...)
	if err := b.db.AddMessage(ctx, chatKey, router.Line{Time: time.Now(), SenderName: b.cfg.BotName, FromBot: true, Text: reply, ID: sentID, To: to}); err != nil {
		log.Printf("store reply: %v", err)
	}
	b.noteMessage(chatKey)
}

// personKey is the profile key: the LID, or "pn:<number>" if no LID is known.
func personKey(lid, pn string) string {
	switch {
	case lid != "":
		return lid
	case pn != "":
		return "pn:" + pn
	}
	return ""
}

// people builds the <people> section: one entry per handle, requester first,
// with their latest name, whitelist status and recent facts (D3, D5).
func (b *bot) people(ctx context.Context, handles map[string]string, history []router.Line, req router.Line, members map[string]member) []router.Person {
	names := map[string]string{}
	wl := map[string]bool{}
	for h, k := range handles { // L5: members known only from the group list
		if m, ok := members[k]; ok {
			names[h], wl[h] = m.Name, m.Whitelisted
		}
	}
	for _, l := range append(append([]router.Line{}, history...), req) {
		if l.Handle == "" {
			continue
		}
		if l.SenderName != "" {
			names[l.Handle] = l.SenderName
		}
		wl[l.Handle] = wl[l.Handle] || l.Whitelisted
	}
	// <people> lists the requester first, then by handle (P5: the requester
	// is no longer always P1).
	order := make([]string, 0, len(handles))
	if handles[req.Handle] != "" {
		order = append(order, req.Handle)
	}
	for i := 1; i <= len(handles); i++ {
		if h := fmt.Sprintf("P%d", i); h != req.Handle {
			order = append(order, h)
		}
	}
	keys := make([]string, 0, len(order))
	for _, h := range order {
		keys = append(keys, handles[h])
	}
	// D7: with tools, Claude searches facts itself and sees only counts;
	// without (CLI), every fact goes inline and FitPrompt trims to 128k.
	var facts map[string][]string
	var counts map[string]int
	var err error
	var profiles map[string]bool
	if b.claude.SupportsTools() {
		counts, err = b.db.FactCounts(ctx, keys)
		if err == nil {
			profiles, err = b.db.HasProfiles(ctx, keys) // V1
		}
	} else {
		facts, err = b.db.PersonFacts(ctx, keys)
	}
	if err != nil {
		log.Printf("person facts: %v", err)
	}
	// K4: everyone's newest states go in every prompt, so a chat isn't stale
	// about what another chat already knows (CLI mode has all facts inline).
	var states map[string][]store.Fact
	if b.claude.SupportsTools() {
		if states, err = b.db.RecentStates(ctx, keys); err != nil {
			log.Printf("states: %v", err)
		}
	}
	out := make([]router.Person, 0, len(keys))
	for i, k := range keys { // keys[0] is the requester
		h := order[i]
		p := router.Person{Handle: h, Name: names[h], Whitelisted: wl[h], Facts: facts[k], FactCount: counts[k], HasProfile: profiles[k]}
		// K1: the requester's standing preferences go in every prompt, even
		// when the other facts are only searchable (D7).
		if h == req.Handle && b.claude.SupportsTools() {
			if p.Preferences, err = b.db.Preferences(ctx, k); err != nil {
				log.Printf("preferences: %v", err)
			}
		}
		if h == req.Handle && b.claude.SupportsTools() { // U1: a nudge to list_tasks
			if p.TaskCounts, err = b.db.TodoCounts(ctx, k); err != nil {
				log.Printf("open tasks: %v", err)
			}
			if p.TaskCats, err = b.db.TodoCategories(ctx, k); err != nil { // U4
				log.Printf("task categories: %v", err)
			}
		}
		for _, f := range states[k] { // K4
			p.States = append(p.States, f.String())
		}
		out = append(out, p)
	}
	return out
}

// applyTags saves what Claude asked to remember, enforcing who may change
// what: memories and person facts from anyone's request (B12, D4), forgets
// and persona changes only from whitelisted requesters (D2, D4).
//
// With tools (J3) memory tags are not applied: memory goes through the
// checked tools, and stray tags are only stripped from the text.
func (b *bot) applyTags(ctx context.Context, chatKey string, req router.Line, handles map[string]string, r router.ParsedReply, tags bool) {
	if !tags {
		if n := len(r.Memories) + len(r.PersonFacts) + len(r.Forgets); n > 0 {
			log.Printf("ignored %d memory tags in tool mode (J3)", n)
		}
		r.Memories, r.PersonFacts, r.Forgets = nil, nil, nil
	}
	if len(r.Memories) > 0 {
		if err := b.db.AddMemories(ctx, chatKey, r.Memories); err != nil {
			log.Printf("save memories: %v", err)
		}
	}
	// resolve maps a tag to a profile key: a handle in this prompt, or any
	// person by name (D4: facts can be saved about anyone, from any chat).
	resolve := func(f router.PersonFact, create bool) (string, string) {
		if f.Handle != "" {
			return handles[f.Handle], f.Handle
		}
		if create {
			p, err := b.db.PersonByName(ctx, f.Name, time.Now())
			if err != nil {
				log.Printf("person %q: %v", f.Name, err)
			}
			return p.Key, f.Name
		}
		if people, _ := b.db.FindPeople(ctx, f.Name, 1); len(people) > 0 {
			return people[0].Key, f.Name
		}
		return "", f.Name
	}
	reqKey := handles[req.Handle]
	for _, f := range r.PersonFacts {
		key, who := resolve(f, true)
		if key == "" {
			log.Printf("person fact for unknown %s ignored", who)
			continue
		}
		src := store.Source{Chat: chatKey, SaidBy: reqKey, Tier: memtools.RequestTier(req, reqKey, key)}
		if err := b.db.AddPersonFacts(ctx, key, src, []string{f.Text}); err != nil {
			log.Printf("save person fact: %v", err)
		} else {
			log.Printf("saved fact for %s (%s, %s)", who, key, src.Tier)
		}
	}
	for _, f := range r.Forgets {
		key, who := resolve(f, false)
		// D4: owners can retire anyone's facts; J4: people their own, up
		// to their own tier.
		tier := memtools.RequestTier(req, reqKey, key)
		if key == "" || tier == store.TierParticipant {
			log.Printf("forget for %s ignored (requester whitelisted=%v)", who, req.Whitelisted)
			continue
		}
		// J1: forgetting invalidates; the fact stays as history.
		if done, err := b.db.InvalidateFact(ctx, key, f.Text, tier, "retracted at "+req.SenderName+"'s request"); err != nil {
			log.Printf("forget for %s: %v", who, err)
		} else if !done {
			log.Printf("forget for %s: no fact matched %q", who, f.Text)
		}
	}
	if r.PersonaSet {
		if !req.Whitelisted {
			log.Printf("persona change from non-whitelisted %s ignored", req.SenderID)
			return
		}
		if err := b.db.SetPersona(ctx, r.Persona, req.SenderID); err != nil {
			log.Printf("save persona: %v", err)
			return
		}
		log.Printf("persona changed by %s", req.SenderID)
	}
}

// send replies in chat, quoting e when given. Every outgoing message waits for
// a slot on the global pacer, so the bot never sends faster than one message
// per SEND_INTERVAL; extra messages queue (B18).
func (b *bot) send(ctx context.Context, chat types.JID, text string, quote *events.Message) string {
	return b.sendMentions(ctx, chat, text, quote, nil)
}

// sendMentions sends text, tagging the given JIDs (L3); their "@user" must
// be in the text. It returns the sent message's id ("" if not sent).
func (b *bot) sendMentions(_ context.Context, chat types.JID, text string, quote *events.Message, mentions []string) string {
	// Own context: a long queue must not inherit the request's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := b.pacer.Wait(ctx); err != nil {
		log.Printf("send to %s dropped: queue wait: %v", chat, err)
		return ""
	}
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text)}}
	if quote != nil {
		msg.ExtendedTextMessage.ContextInfo = &waE2E.ContextInfo{
			StanzaID:      proto.String(quote.Info.ID),
			Participant:   proto.String(quote.Info.Sender.ToNonAD().String()),
			QuotedMessage: quote.Message,
		}
	}
	if len(mentions) > 0 {
		if msg.ExtendedTextMessage.ContextInfo == nil {
			msg.ExtendedTextMessage.ContextInfo = &waE2E.ContextInfo{}
		}
		msg.ExtendedTextMessage.ContextInfo.MentionedJID = mentions
	}
	resp, err := b.wa.SendMessage(ctx, chat, msg)
	if err != nil {
		log.Printf("send to %s: %v", chat, err)
		return ""
	}
	return resp.ID
}

// ---- B15: only stay in groups a whitelisted member added us to and that
// still contain a whitelisted member. ----

func (b *bot) hasWhitelistedMember(ctx context.Context, g *types.GroupInfo) bool {
	for _, p := range g.Participants {
		lid := p.LID
		if p.JID.Server == types.HiddenUserServer {
			lid = p.JID
		}
		if b.whitelisted(ctx, lid, p.PhoneNumber) {
			return true
		}
	}
	return false
}

func (b *bot) leave(ctx context.Context, g types.JID, why string) {
	log.Printf("leaving group %s: %s", g, why)
	if err := b.wa.LeaveGroup(ctx, g); err != nil {
		log.Printf("leave %s: %v", g, err)
	}
}

func (b *bot) checkAdded(ctx context.Context, g *types.GroupInfo, sender, senderPN *types.JID) {
	var s, spn types.JID
	if sender != nil {
		s = *sender
	}
	if senderPN != nil {
		spn = *senderPN
	}
	if !b.whitelisted(ctx, s, spn) {
		b.leave(ctx, g.JID, fmt.Sprintf("added by non-whitelisted %v", s))
		return
	}
	if !b.hasWhitelistedMember(ctx, g) {
		b.leave(ctx, g.JID, "no whitelisted member")
	}
}

func (b *bot) onJoined(e *events.JoinedGroup) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	b.checkAdded(ctx, &e.GroupInfo, e.Sender, e.SenderPN)
}

func (b *bot) isMe(j types.JID) bool {
	if id := b.wa.Store.ID; id != nil && j.User == id.User && j.Server == types.DefaultUserServer {
		return true
	}
	lid := b.wa.Store.GetLID()
	return j.Server == types.HiddenUserServer && j.User == lid.User
}

func (b *bot) onGroupChange(e *events.GroupInfo) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, j := range e.Join {
		if b.isMe(j) {
			g, err := b.wa.GetGroupInfo(ctx, e.JID)
			if err != nil {
				log.Printf("group info %s: %v", e.JID, err)
				return
			}
			b.checkAdded(ctx, g, e.Sender, e.SenderPN)
			return
		}
	}
	if len(e.Leave) > 0 {
		g, err := b.wa.GetGroupInfo(ctx, e.JID)
		if err != nil {
			return // probably we left or were removed
		}
		if !b.hasWhitelistedMember(ctx, g) {
			b.leave(ctx, e.JID, "last whitelisted member left")
		}
	}
}

// chatResolver finds another chat an owner may schedule into (J10): "dm"
// for their own private chat, or a group they are in, by name.
func (b *bot) chatResolver(lid, pn string) func(ctx context.Context, name string) (string, string, error) {
	me := func(u types.JID) bool { return u.User != "" && (u.User == lid || u.User == pn) }
	return func(ctx context.Context, name string) (string, string, error) {
		switch strings.ToLower(strings.TrimSpace(name)) {
		case "dm", "me", "private", "my dm", "pm":
			switch {
			case pn != "":
				return types.NewJID(pn, types.DefaultUserServer).String(), "your DM", nil
			case lid != "":
				return types.NewJID(lid, types.HiddenUserServer).String(), "your DM", nil
			}
			return "", "", errors.New("I don't know your number")
		}
		groups, err := b.wa.GetJoinedGroups(ctx)
		if err != nil {
			return "", "", errors.New("could not list groups")
		}
		var hits []*types.GroupInfo
		for _, g := range groups {
			in := false
			for _, p := range g.Participants {
				in = in || me(p.JID) || me(p.LID) || me(p.PhoneNumber)
			}
			if !in {
				continue
			}
			switch n, q := strings.ToLower(g.Name), strings.ToLower(strings.TrimSpace(name)); {
			case n == q:
				return g.JID.String(), g.Name, nil
			case strings.Contains(n, q):
				hits = append(hits, g)
			}
		}
		switch len(hits) {
		case 1:
			return hits[0].JID.String(), hits[0].Name, nil
		case 0:
			return "", "", fmt.Errorf("no group of yours is called %q", name)
		}
		var names []string
		for _, g := range hits {
			names = append(names, g.Name)
		}
		return "", "", fmt.Errorf("several groups match: %s", strings.Join(names, ", "))
	}
}

// kickEmbedder wakes the background embedder (J2 c); it never blocks.
func (b *bot) kickEmbedder() {
	if b.embedKick == nil {
		return
	}
	select {
	case b.embedKick <- struct{}{}:
	default:
	}
}

// runEmbedder gives every fact an embedding vector (J2 c): at start, a few
// seconds after saves, and every 10 minutes for anything missed.
func (b *bot) runEmbedder() {
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
		total := 0
		for {
			n, err := memtools.Embed(ctx, b.db, b.vectors.E, 100)
			if err != nil {
				log.Printf("embedder: %v", err)
				break
			}
			if total += n; n == 0 {
				break
			}
		}
		cancel()
		if total > 0 {
			log.Printf("embedder: embedded %d facts", total)
		}
		select {
		case <-b.embedKick:
			time.Sleep(5 * time.Second) // batch saves made in one reply
		case <-tick.C:
		}
	}
}

// runScheduler sends scheduled messages when due (J7), through the pacer
// (B18). Jobs live in SQLite, so they survive restarts (J10: a one-off sent
// late if under 12 h late; a recurring one skips missed runs).
func (b *bot) runScheduler() {
	const (
		tick        = 30 * time.Second
		oneOffGrace = 12 * time.Hour
		repeatGrace = 30 * time.Minute
	)
	for {
		b.runDue(time.Now(), oneOffGrace, repeatGrace)
		b.runTaskReminders(time.Now()) // U3
		time.Sleep(tick)
	}
}

// runTaskReminders reminds owners of tasks coming due (U3). Code decides
// when (store.Todo.RemindAt); the message is written now, like a scheduled
// one (T1). It goes to the owner's DM if they may DM the bot (B3), else to
// the chat the task was added in, tagging them.
func (b *bot) runTaskReminders(now time.Time) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	due, err := b.db.TodoRemindersDue(ctx, now)
	if err != nil {
		log.Printf("task reminders: %v", err)
		return
	}
	for _, td := range due {
		// Marked first: a reminder goes out at most once per due time.
		if err := b.db.MarkTodoReminded(ctx, td.ID, td.Due); err != nil {
			log.Printf("task reminders: task %d: %v", td.ID, err)
			continue
		}
		acct, err := b.db.PersonAccount(ctx, td.Owner)
		if err != nil {
			log.Printf("task reminders: task %d: %v", td.ID, err)
			continue
		}
		var chat types.JID
		dm := b.allowed(acct.LID, acct.PN)
		switch {
		case dm && acct.PN != "":
			chat = types.NewJID(acct.PN, types.DefaultUserServer)
		case dm && acct.LID != "":
			chat = types.NewJID(acct.LID, types.HiddenUserServer)
		case td.Chat != "":
			dm = false
			if chat, err = types.ParseJID(td.Chat); err != nil {
				log.Printf("task reminders: task %d: bad chat", td.ID)
				continue
			}
		default:
			log.Printf("task reminders: task %d: nowhere to send", td.ID)
			continue
		}
		brief := router.TaskReminderBrief(td.Name, td.Description, td.Due, now, dm, td.Owner)
		log.Printf("task reminders: task %d due %s", td.ID, td.Due.Format(time.RFC3339))
		b.sendJob(ctx, store.Job{Chat: chat.String(), CreatedBy: td.Owner, CreatedName: acct.Name, Text: brief}, chat)
	}
}

func (b *bot) runDue(now time.Time, oneOffGrace, repeatGrace time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	jobs, err := b.db.DueJobs(ctx, now)
	if err != nil {
		log.Printf("scheduler: %v", err)
		return
	}
	for _, j := range jobs {
		send, next := memtools.DueAction(j, now, oneOffGrace, repeatGrace)
		late := now.Sub(j.NextAt)
		if !next.IsZero() {
			if err := b.db.RescheduleJob(ctx, j.ID, next); err != nil {
				log.Printf("scheduler: job %d: %v", j.ID, err)
				continue
			}
		} else if err := b.db.DeleteJob(ctx, j.ID); err != nil {
			log.Printf("scheduler: job %d: %v", j.ID, err)
			continue
		}
		if !send {
			log.Printf("scheduler: job %d skipped, %s late", j.ID, late.Round(time.Minute))
			continue
		}
		chat, err := types.ParseJID(j.Chat)
		if err != nil {
			log.Printf("scheduler: job %d: bad chat", j.ID)
			continue
		}
		b.sendJob(ctx, j, chat)
	}
}

// sendJob writes a due job's message now, from its brief and the chat's
// current context (T1), and sends it; on a model error, an empty answer or
// the M3 cap the brief goes out as written. L10: pinned "@{key}" tags in the
// brief become real mentions.
func (b *bot) sendJob(ctx context.Context, j store.Job, chat types.JID) {
	unlock := b.lock(j.Chat)
	defer unlock()
	out, stored, mentions := router.ExpandMentions(j.Text, b.nameOf(ctx))
	how := "as written"
	if text, ok := b.composeJob(ctx, j, chat); ok {
		out, stored, mentions = text.send, text.stored, text.mentions
		how = "written now"
	}
	sentID := b.sendMentions(ctx, chat, out, nil, mentionJIDs(mentions))
	log.Printf("scheduler: sent job %d %s (%d mentions)", j.ID, how, len(mentions))
	if err := b.db.AddMessage(ctx, j.Chat, router.Line{Time: time.Now(), SenderName: b.cfg.BotName, FromBot: true, Text: stored, ID: sentID,
		To: b.mentionNames(ctx, mentions, "")}); err != nil { // K7
		log.Printf("scheduler: store: %v", err)
	}
	if j.Repeat != "" {
		if err := b.db.NoteJobSent(ctx, j.ID, stored); err != nil {
			log.Printf("scheduler: job %d: note sent: %v", j.ID, err)
		}
	}
}

type composed struct {
	send, stored string
	mentions     []router.Mention
}

// composeJob asks the model for a due job's message (T1), with the same
// context a reply in that chat gets, read-only memory tools and web search.
// ok is false when the brief should be sent as written instead.
func (b *bot) composeJob(ctx context.Context, j store.Job, chat types.JID) (composed, bool) {
	if !b.claude.SupportsTools() {
		return composed{}, false // CLI mode: no tools; keep the brief
	}
	if b.overBudget(ctx) {
		log.Printf("scheduler: job %d: over the daily budget, brief as written", j.ID)
		return composed{}, false // M3
	}
	chatKey := j.Chat
	isGroup := chat.Server == types.GroupServer
	history, err := b.db.History(ctx, chatKey)
	if err != nil {
		log.Printf("scheduler: history: %v", err)
	}
	summary, err := b.db.Summary(ctx, chatKey)
	if err != nil {
		log.Printf("scheduler: summary: %v", err)
	}
	var reminders []router.Reminder // N3: the others still pending
	if jobs, err := b.db.ListJobs(ctx, chatKey, ""); err == nil {
		for _, o := range jobs {
			if o.ID == j.ID {
				continue
			}
			_, text, _ := router.ExpandMentions(o.Text, b.nameOf(ctx))
			reminders = append(reminders, router.Reminder{At: o.NextAt, Text: text, Repeat: o.Repeat})
		}
	}
	// The request is the creator's (their standing preferences apply), or
	// nobody's for the bot's own idea.
	req := router.Line{Time: time.Now(), SenderName: j.CreatedName, SenderID: j.CreatedBy}
	if k := j.CreatedBy; k != "" {
		if pn, ok := strings.CutPrefix(k, "pn:"); ok {
			req.Whitelisted = b.allowed("", pn)
		} else {
			req.Whitelisted = b.allowed(k, "")
		}
	}
	if j.SelfInitiated || req.SenderName == "" {
		req = router.Line{Time: req.Time, SenderName: b.cfg.BotName + " (your own scheduled message)"}
	}
	handles := router.AssignHandles(history, &req)
	var members map[string]member
	if isGroup {
		var keys []string
		keys, members = b.groupMembers(ctx, chat)
		router.AddHandles(handles, keys)
	}
	// Tagged people get handles even if they haven't spoken lately.
	var tagged []string
	_, _, ms := router.ExpandMentions(j.Text, b.nameOf(ctx))
	for _, m := range ms {
		tagged = append(tagged, m.Key)
	}
	router.AddHandles(handles, tagged)
	people := b.people(ctx, handles, history, req, members)
	names := map[string]string{}
	for _, p := range people {
		names[p.Handle] = p.Name
	}
	for h, k := range handles {
		if names[h] == "" {
			names[h] = b.nameOf(ctx)(k)
		}
	}
	brief := router.UnpinMentions(j.Text, handles, b.nameOf(ctx))
	text := router.ScheduledRequest(brief, j.Repeat, j.Sent)
	mems, err := b.db.SearchMemories(ctx, chatKey, brief, b.cfg.MemoryResults)
	if err != nil {
		log.Printf("scheduler: memories: %v", err)
	}
	turn := &memtools.Turn{DB: b.db, Chat: chatKey, Handles: handles, Names: names, Requester: req, Vectors: b.vectors}
	toolset := turn.ReadTools() // T1: read-only memory; web search comes with the client
	toolBytes := 0
	for _, t := range toolset {
		js, _ := json.Marshal(t.Schema)
		toolBytes += len(t.Name) + len(t.Description) + len(js)
	}
	persona, err := b.db.Persona(ctx)
	if err != nil {
		log.Printf("scheduler: persona: %v", err)
	}
	system := router.SystemPrompt(b.cfg.BotName, persona, true)
	full := router.FitPromptWith(system, router.Prompt{
		Memories: mems, Summary: summary, Reminders: reminders, People: people, History: history, Requester: req, Text: text, Now: time.Now(), Group: isGroup,
	}, b.cfg.ContextTokens, b.ratio.BytesPerToken(), toolBytes)
	var st llm.Stats
	reply, err := b.claude.Reply(llm.WithStats(ctx, &st), system, full, nil, toolset)
	b.account("scheduled "+chatKey, st)
	if err != nil {
		log.Printf("scheduler: job %d: compose: %v; brief as written", j.ID, err)
		return composed{}, false
	}
	reply = strings.TrimSpace(router.ParseReply(reply).Text) // tags (persona) ignored here
	if reply == "" {
		log.Printf("scheduler: job %d: empty compose; brief as written", j.ID)
		return composed{}, false
	}
	send, stored, mentions := router.ResolveMentions(reply, handles, names)
	return composed{send, stored, mentions}, true
}

// guessMerges folds name-only profiles into the real account they belong
// to (J12): a unique name or nickname match who is in a chat the facts came
// from (a group member, or the other side of a DM).
func (b *bot) guessMerges() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	profiles, err := b.db.NameProfiles(ctx)
	if err != nil {
		log.Printf("guess merges: %v", err)
		return
	}
	groups := map[string]*types.GroupInfo{}
	inChat := func(chat string, a store.Account) bool {
		j, err := types.ParseJID(chat)
		if err != nil {
			return false
		}
		is := func(u types.JID) bool {
			return u.User != "" && (u.User == a.LID || u.User == a.PN)
		}
		if j.Server != types.GroupServer {
			return is(j) // a DM: the chat is the person
		}
		g, ok := groups[chat]
		if !ok {
			if g, err = b.wa.GetGroupInfo(ctx, j); err != nil {
				g = nil // left the group: no evidence
			}
			groups[chat] = g
		}
		if g == nil {
			return false
		}
		for _, p := range g.Participants {
			if is(p.JID) || is(p.LID) || is(p.PhoneNumber) {
				return true
			}
		}
		return false
	}
	for _, p := range profiles {
		cands, err := b.db.MergeCandidates(ctx, p.Name)
		if err != nil || len(cands) == 0 {
			continue
		}
		a, ok := store.PickMerge(p, cands, inChat)
		if !ok {
			continue
		}
		if done, err := b.db.MergePerson(ctx, p.Key, a.Key); err != nil || !done {
			continue
		}
		if !strings.EqualFold(p.Name, a.Name) { // keep the name findable
			b.db.AddPersonFacts(ctx, a.Key, store.Source{Tier: store.TierInferred}, []string{"Alias: " + p.Name})
		}
		log.Printf("merged %s into %s (#%s), guessed from name and chat", p.Key, a.Name, store.IDTag(a.Key))
	}
}

// auditGroups leaves groups without a whitelisted member at startup.
func (b *bot) auditGroups() {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	groups, err := b.wa.GetJoinedGroups(ctx)
	if err != nil {
		log.Printf("list groups: %v", err)
		return
	}
	for _, g := range groups {
		if !b.hasWhitelistedMember(ctx, g) {
			b.leave(ctx, g.JID, "no whitelisted member")
		}
	}
}

// newClient builds the model client for the configured provider (A5, G1).
func newClient(cfg *config.Config, web, announce bool) llm.Client {
	logf := func(f string, a ...any) {
		if announce {
			log.Printf(f, a...)
		}
	}
	switch {
	case cfg.Provider == "openrouter":
		logf("model via OpenRouter, model %s, effort %s, web tools %v", cfg.Model, cfg.Effort, web)
		return llm.NewOpenRouter(cfg.OpenRouterKey, cfg.Model, cfg.Effort, web)
	case cfg.AnthropicAPIKey != "":
		logf("Claude via Messages API, model %s, effort %s, web tools %v", cfg.Model, cfg.Effort, web)
		return llm.NewAPI(cfg.AnthropicAPIKey, cfg.Model, cfg.Effort, web)
	default:
		logf("Claude via %s CLI (subscription token), model %s, effort %s", cfg.ClaudeBin, cfg.Model, cfg.Effort)
		return llm.NewCLI(cfg.ClaudeBin, cfg.OAuthToken, cfg.Model, cfg.Effort, filepath.Join(cfg.DataDir, "claude-work"), web)
	}
}

// ---- Knowledge curator (H1-H3) ----

const (
	curateEvery = 16               // H1: a pass per 16 unread messages in a chat
	curateIdle  = 10 * time.Minute // H1: or after this much quiet, if any are unread
)

// noteMessage runs after every stored message: a curator pass when the chat
// has curateEvery unread messages, otherwise (re)arm the idle timer. Messages
// are taken synchronously, before the 16-message window can prune them.
func (b *bot) noteMessage(chat string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	if b.overBudget(ctx) {
		cancel()
		return // M3: messages stay unread until tomorrow's budget
	}
	lines, err := b.db.TakeUnread(ctx, chat, curateEvery)
	cancel()
	if err != nil {
		log.Printf("curator: take unread: %v", err)
		return
	}
	b.curIdleMu.Lock()
	defer b.curIdleMu.Unlock()
	if t := b.curIdle[chat]; t != nil {
		t.Stop()
		delete(b.curIdle, chat)
	}
	if len(lines) > 0 {
		go b.curate(chat, lines)
		return
	}
	b.curIdle[chat] = time.AfterFunc(curateIdle, func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if b.overBudget(ctx) {
			cancel()
			return // M3
		}
		lines, err := b.db.TakeUnread(ctx, chat, 1)
		cancel()
		if err != nil {
			log.Printf("curator: take unread: %v", err)
			return
		}
		if len(lines) > 0 {
			b.curate(chat, lines)
		}
	})
}

// curate is one background pass: show the model the new messages and what is
// known about the people in them, then save the new facts and nicknames (H1).
func (b *bot) curate(chat string, lines []router.Line) {
	b.curMu.Lock()
	defer b.curMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	handles := router.AssignHandles(lines, &router.Line{})
	if len(handles) == 0 {
		return // only the bot's own messages
	}
	keys := make([]string, 0, len(handles))
	for h := range handles {
		keys = append(keys, handles[h])
	}
	facts, err := b.db.PersonFacts(ctx, keys)
	if err != nil {
		log.Printf("curator: facts: %v", err)
		return
	}
	var people []router.Person
	for i := 1; i <= len(handles); i++ {
		h := fmt.Sprintf("P%d", i)
		k := handles[h]
		name, _ := b.db.PersonName(ctx, k)
		f := facts[k]
		if len(f) > 40 { // newest 40 are enough to avoid repeats
			f = f[len(f)-40:]
		}
		prof, err := b.db.GetProfile(ctx, k) // V1: the curator decides whether to rewrite it
		if err != nil {
			log.Printf("curator: profile: %v", err)
		}
		people = append(people, router.Person{Handle: h, Name: name, Facts: f, Profile: prof.Text})
	}
	names := map[string]string{}
	for _, p := range people {
		names[p.Handle] = p.Name
	}
	old, err := b.db.Summary(ctx, chat)
	if err != nil {
		log.Printf("curator: summary: %v", err)
	}
	prompt := router.CuratorPrompt(people, lines, old)
	var saved int
	var out string
	if b.curator.SupportsTools() { // J3: the curator saves with the same checked tool
		turn := &memtools.Turn{DB: b.db, Chat: chat, Handles: handles, Names: names, AfterSave: b.kickEmbedder}
		var st llm.Stats
		out, err = b.curator.Reply(llm.WithStats(ctx, &st), router.CuratorSystemTools, prompt, nil, turn.CuratorTools())
		b.account("curator "+chat, st)
		if err != nil {
			log.Printf("curator: %v", err)
			return
		}
		saved = turn.Saved()
	} else {
		if out, err = b.curator.Reply(ctx, router.CuratorSystem, prompt, nil, nil); err != nil {
			log.Printf("curator: %v", err)
			return
		}
		saved = b.curateTags(ctx, chat, handles, router.ParseCurator(out))
	}
	// J8: the same pass rewrites the chat's rolling summary.
	summary := router.ParseSummaryFor(out, names) // K6: no per-prompt handles kept
	if summary != "" {
		if err := b.db.SetSummary(ctx, chat, summary); err != nil {
			log.Printf("curator: save summary: %v", err)
		}
	}
	log.Printf("curator: %s read %d messages, saved %d facts/aliases, summary %d words", chat, len(lines), saved, len(strings.Fields(summary)))
	if saved > 0 {
		b.guessMerges() // new name-only profiles may belong to someone known
	}
}

// curateTags applies a curator pass's tags (CLI mode, which has no custom
// tools) and reports how many facts it saved.
func (b *bot) curateTags(ctx context.Context, chat string, handles map[string]string, r router.CuratorResult) int {
	saved := 0
	inferred := store.Source{Chat: chat, Tier: store.TierInferred} // J4
	for _, f := range r.Facts {
		key := handles[f.Handle]
		if f.Handle == "" {
			p, err := b.db.PersonByName(ctx, f.Name, time.Now())
			if err != nil {
				continue
			}
			key = p.Key
		}
		if key == "" {
			continue
		}
		if err := b.db.AddPersonFacts(ctx, key, inferred, []string{f.Text}); err == nil {
			saved++
		}
	}
	for _, a := range r.Aliases {
		key := handles[a.Handle]
		if key == "" {
			continue
		}
		if err := b.db.AddPersonFacts(ctx, key, inferred, []string{"Alias: " + a.Text}); err == nil {
			saved++
		}
		// H3: a name-only profile under this nickname belongs to them.
		from := "name:" + strings.ToLower(strings.Join(strings.Fields(a.Text), " "))
		if ok, err := b.db.MergePerson(ctx, from, key); err != nil {
			log.Printf("curator: merge %s: %v", from, err)
		} else if ok {
			log.Printf("curator: merged %s into %s", from, key)
		}
	}
	return saved
}

const keyUsage = "Usage:\n- /key: list hosts with a stored key\n- /key <host> <token>: save it as a Bearer token\n- /key <host> <Header-Name>: <value>: save a custom header\n- /key <host> remove: delete it"

var headerName = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)

// command runs an owner command (L1, L2). Commands from anyone else are
// dropped without a reply; the message is never stored either way.
func (b *bot) command(ctx context.Context, e *events.Message, cmd router.Command, isGroup, wl bool) {
	chat := e.Info.Chat
	if !wl {
		log.Printf("ignored /%s from non-owner %s", cmd.Name, e.Info.Sender)
		return
	}
	switch cmd.Name {
	case "reset":
		chatKey := chat.String()
		unlock := b.lock(chatKey)
		defer unlock()
		b.curIdleMu.Lock()
		if t := b.curIdle[chatKey]; t != nil {
			t.Stop()
			delete(b.curIdle, chatKey)
		}
		b.curIdleMu.Unlock()
		// Let the curator read what's unread first, so no facts are lost.
		if lines, err := b.db.TakeUnread(ctx, chatKey, 1); err != nil {
			log.Printf("reset: take unread: %v", err)
		} else if len(lines) > 0 {
			b.curate(chatKey, lines)
		}
		n, err := b.db.ResetChat(ctx, chatKey)
		if err != nil {
			log.Printf("reset %s: %v", chatKey, err)
			b.send(ctx, chat, "Reset failed; nothing was cleared.", e)
			return
		}
		log.Printf("reset %s: dropped %d messages and the summary", chatKey, n)
		b.send(ctx, chat, "Chat context cleared: I start fresh from here. Facts, memories and reminders are kept.", e)
	case "key":
		if isGroup {
			log.Printf("refused /key in group %s", chat)
			b.send(ctx, chat, "Not here: everyone in this group can read that. Revoke that token now, then send /key to me in a DM.", e)
			return
		}
		if e.Info.IsFromMe || b.newHTTPTool == nil {
			if b.newHTTPTool == nil {
				b.send(ctx, chat, "API tools are off (API_TOOLS=false), so keys can't be used.", e)
			}
			return
		}
		b.send(ctx, chat, b.keyCommand(cmd.Args), e)
	case "mute", "unmute":
		b.send(ctx, chat, b.muteCommand(ctx, e, cmd), e)
	}
}

// keyCommand applies /key and returns the reply. It never echoes a secret.
func (b *bot) keyCommand(args []string) string {
	b.keysMu.Lock()
	defer b.keysMu.Unlock()
	if len(args) == 0 {
		if len(b.keys) == 0 {
			return "No stored keys.\n\n" + keyUsage
		}
		return "Stored keys for: " + strings.Join(b.keys.Hosts(), ", ")
	}
	host := tools.NormalizeHost(args[0])
	if host == "" || len(args) < 2 {
		return keyUsage
	}
	next := tools.Keys{}
	for h, hs := range b.keys {
		next[h] = hs
	}
	var done string
	switch {
	case len(args) == 2 && strings.EqualFold(args[1], "remove"):
		if next[host] == nil {
			return "No key stored for " + host + "."
		}
		delete(next, host)
		done = "Removed the key for " + host + "."
	case strings.HasSuffix(args[1], ":") && len(args) >= 3 && headerName.MatchString(strings.TrimSuffix(args[1], ":")):
		next[host] = map[string]string{strings.TrimSuffix(args[1], ":"): strings.Join(args[2:], " ")}
		done = "Saved a key for " + host + " (header " + strings.TrimSuffix(args[1], ":") + ")."
	case len(args) == 2:
		next[host] = map[string]string{"Authorization": "Bearer " + args[1]}
		done = "Saved a key for " + host + " (Bearer token)."
	default:
		return keyUsage
	}
	if err := tools.SaveKeys(b.keysPath, next); err != nil {
		log.Printf("save api keys: %v", err)
		return "Couldn't save the key file; nothing changed."
	}
	b.keys = next
	t := b.newHTTPTool(next)
	b.httpOwner.Store(&t)
	log.Printf("api keys: %s (%d hosts)", done, len(next))
	if !strings.HasPrefix(done, "Removed") {
		done += " Now delete your message with the token from this chat."
	}
	return done
}

// logStats logs what one model request cost (L4), if the client reported
// it, with the day's total so far (M3) when known.
func logStats(what string, st llm.Stats, day float64) {
	if st.Rounds == 0 {
		return
	}
	extra := ""
	if st.WebSearches > 0 {
		extra = fmt.Sprintf(", %d web searches", st.WebSearches)
	}
	if day > 0 {
		extra += fmt.Sprintf("; today $%.4f of $%.2f", day, dailyBudget)
	}
	via := ""
	if len(st.Providers) > 0 { // P1
		via = " via " + strings.Join(st.Providers, ",")
	}
	log.Printf("cost %s: $%.5f, %d rounds, %d prompt (%d cached) + %d output tokens%s%s", what, st.Cost, st.Rounds, st.PromptTokens, st.CachedTokens, st.OutputTokens, via, extra)
}

// ---- L5: group member awareness ----

// MaxMembers caps the extra group members shown in a prompt.
const MaxMembers = 40

const membersTTL = 10 * time.Minute

type member struct {
	Name        string
	Whitelisted bool
}

type memberCache struct {
	at      time.Time
	keys    []string
	members map[string]member
}

// groupMembers lists a group's members with a known name (profile name
// first, else their WhatsApp push name), people the bot already knows first,
// at most MaxMembers, without the bot. Cached for membersTTL.
func (b *bot) groupMembers(ctx context.Context, chat types.JID) ([]string, map[string]member) {
	b.membersMu.Lock()
	if c, ok := b.members[chat.String()]; ok && time.Since(c.at) < membersTTL {
		b.membersMu.Unlock()
		return c.keys, c.members
	}
	b.membersMu.Unlock()
	g, err := b.wa.GetGroupInfo(ctx, chat)
	if err != nil {
		log.Printf("group members %s: %v", chat, err)
		return nil, nil
	}
	var known, other []string
	members := map[string]member{}
	for _, p := range g.Participants {
		if b.isMe(p.JID) || (!p.LID.IsEmpty() && b.isMe(p.LID)) {
			continue
		}
		alt := p.LID
		if alt.IsEmpty() {
			alt = p.PhoneNumber
		}
		lid, pn := b.identify(ctx, p.JID, alt)
		key := personKey(lid, pn)
		if key == "" {
			continue
		}
		name, _ := b.db.PersonName(ctx, key)
		isKnown := name != ""
		if name == "" {
			for _, j := range []types.JID{p.JID, p.LID, p.PhoneNumber} {
				if j.IsEmpty() {
					continue
				}
				if c, err := b.wa.Store.Contacts.GetContact(ctx, j); err == nil && c.Found {
					for _, n := range []string{c.FullName, c.PushName, c.FirstName} {
						if strings.TrimSpace(n) != "" {
							name = n
							break
						}
					}
				}
				if name != "" {
					break
				}
			}
		}
		if name == "" {
			continue // a handle without a name helps nobody
		}
		members[key] = member{Name: name, Whitelisted: b.allowed(lid, pn)}
		if isKnown {
			known = append(known, key)
		} else {
			other = append(other, key)
		}
	}
	keys := append(known, other...)
	if len(keys) > MaxMembers {
		keys = keys[:MaxMembers]
	}
	b.membersMu.Lock()
	if b.members == nil {
		b.members = map[string]memberCache{}
	}
	b.members[chat.String()] = memberCache{at: time.Now(), keys: keys, members: members}
	b.membersMu.Unlock()
	return keys, members
}

// onReaction records an emoji reaction on the message it targets (L6). It
// never triggers a reply. Only messages still in the chat's window count,
// and in DMs only owners' reactions (others get no conversation anyway).
func (b *bot) onReaction(ctx context.Context, e *events.Message, rm *waE2E.ReactionMessage) {
	if e.Info.IsFromMe {
		return
	}
	chat := e.Info.Chat
	lid, pn := b.identify(ctx, e.Info.Sender, e.Info.SenderAlt)
	if chat.Server != types.GroupServer && !b.allowed(lid, pn) {
		return
	}
	key := personKey(lid, pn)
	if !b.allowed(lid, pn) && b.isMuted(ctx, key) {
		return // M2
	}
	name := e.Info.PushName
	if n, _ := b.db.PersonName(ctx, key); n != "" {
		name = n
	}
	ok, err := b.db.SetReaction(ctx, chat.String(), rm.GetKey().GetID(), key, name, rm.GetText(), e.Info.Timestamp)
	switch {
	case err != nil:
		log.Printf("reaction in %s: %v", chat, err)
	case ok:
		log.Printf("reaction in %s by %s: %q", chat, key, rm.GetText())
	}
}

// mentionJIDs are the WhatsApp JIDs of tagged people (L3): LIDs, or phone
// numbers for "pn:" profiles.
func mentionJIDs(ms []router.Mention) []string {
	jids := make([]string, 0, len(ms))
	for _, m := range ms {
		server := types.HiddenUserServer
		if strings.HasPrefix(m.Key, "pn:") {
			server = types.DefaultUserServer
		}
		jids = append(jids, types.NewJID(m.User, server).String())
	}
	return jids
}

// addressees is K7: who an incoming message is addressed to, only where the
// message itself says so. Every DM message is to the bot; in a group, a reply
// is to the quoted message's writer, and a tag to the tagged person.
func (b *bot) addressees(ctx context.Context, m *waE2E.Message, isGroup bool, quotedName, sender string) (toBot bool, to []string) {
	if !isGroup {
		return true, nil
	}
	seen := map[string]bool{}
	add := func(n string) {
		if n = strings.TrimSpace(n); n != "" && !seen[n] {
			seen[n] = true
			to = append(to, n)
		}
	}
	ci := contextInfo(m)
	if ci == nil {
		return false, nil
	}
	if ci.GetQuotedMessage() != nil {
		if j, err := types.ParseJID(ci.GetParticipant()); err == nil && b.isMe(j) {
			toBot = true
		} else {
			add(quotedName)
		}
	}
	for _, s := range ci.GetMentionedJID() {
		j, err := types.ParseJID(s)
		if err != nil {
			continue
		}
		if b.isMe(j) {
			toBot = true
			continue
		}
		lid, pn := b.identify(ctx, j, types.EmptyJID)
		if k := personKey(lid, pn); k != "" && k != sender {
			n, _ := b.db.PersonName(ctx, k)
			add(n)
		}
	}
	return toBot, to
}

// mentionNames are the names of tagged people, except skip (K7).
func (b *bot) mentionNames(ctx context.Context, ms []router.Mention, skip string) []string {
	var out []string
	for _, m := range ms {
		if m.Key == skip {
			continue
		}
		if n, _ := b.db.PersonName(ctx, m.Key); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// nameOf looks up a person's display name by key, for "@Name" in history.
func (b *bot) nameOf(ctx context.Context) func(key string) string {
	return func(key string) string {
		n, _ := b.db.PersonName(ctx, key)
		return n
	}
}
