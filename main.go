// Command wagoya is a WhatsApp number backed by an LLM (A1).
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
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

	wlMu  sync.RWMutex
	wlLID map[string]bool // configured LIDs plus LIDs resolved from phone numbers
}

func main() {
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
		pacer: ratelimit.NewPacer(cfg.SendInterval), denyCool: ratelimit.NewCooldown(cfg.DenyCooldown)}
	for l := range cfg.WhitelistLID {
		b.wlLID[l] = true
	}
	if cfg.APITools {
		h := tools.HTTPTool(tools.NewHTTPClient())
		b.http, b.mcp = &h, mcpTools.Tools()
		log.Printf("api tools: http_request for everyone, %d MCP tools for whitelisted requesters", len(b.mcp))
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
	text, img := messageText(e.Message)
	if text == "" && img == nil {
		return // B8: other media ignored
	}
	isGroup := chat.Server == types.GroupServer
	lid, pn := b.identify(ctx, e.Info.Sender, e.Info.SenderAlt)
	wl := b.allowed(lid, pn)

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

	stored := text
	if img != nil {
		stored = strings.TrimSpace("[image] " + text)
	}
	qName, qText, qImg := b.quoted(ctx, e.Message)
	stored = router.WithQuote(stored, qName, qText, 300)
	prompt := router.WithQuote(d.Prompt, qName, qText, 4000)
	key := personKey(lid, pn)
	line := router.Line{Time: e.Info.Timestamp, SenderName: e.Info.PushName, SenderID: key, Whitelisted: wl, Text: stored}
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

	history, err := b.db.History(ctx, chatKey) // window before this message
	if err != nil {
		log.Printf("history: %v", err)
	}
	if err := b.db.AddMessage(ctx, chatKey, line); err != nil {
		log.Printf("store message: %v", err)
	}
	b.noteMessage(chatKey)
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
	system := router.SystemPrompt(b.cfg.BotName, persona)
	handles := router.AssignHandles(history, &line)
	people := b.people(ctx, handles, history, line)

	_ = b.wa.SendChatPresence(ctx, chat, types.ChatPresenceComposing, types.ChatPresenceMediaText)
	full := router.FitPrompt(system, router.Prompt{
		Memories: mems, People: people, History: history, Requester: line, Text: prompt, HasImage: image != nil, Now: time.Now(),
	}, b.cfg.ContextTokens)
	toolset := []llm.Tool{llm.FactSearchTool(b.factSearch(handles))}
	if b.http != nil {
		toolset = append(toolset, *b.http)
	}
	if line.Whitelisted { // I2: MCP tools are owner-only
		toolset = append(toolset, b.mcp...)
	}
	reply, err := b.claude.Reply(ctx, system, full, image, toolset)
	_ = b.wa.SendChatPresence(ctx, chat, types.ChatPresencePaused, types.ChatPresenceMediaText)
	if err != nil {
		log.Printf("claude: %v", err)
		b.send(ctx, chat, "Sorry, I couldn't reach Claude right now.", e)
		return
	}
	parsed := router.ParseReply(reply)
	b.applyTags(ctx, chatKey, line, handles, parsed)
	reply = parsed.Text
	if reply == "" {
		return
	}
	b.send(ctx, chat, reply, e)
	if err := b.db.AddMessage(ctx, chatKey, router.Line{Time: time.Now(), SenderName: b.cfg.BotName, FromBot: true, Text: reply}); err != nil {
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
func (b *bot) people(ctx context.Context, handles map[string]string, history []router.Line, req router.Line) []router.Person {
	names := map[string]string{}
	wl := map[string]bool{}
	for _, l := range append(append([]router.Line{}, history...), req) {
		if l.Handle == "" {
			continue
		}
		if l.SenderName != "" {
			names[l.Handle] = l.SenderName
		}
		wl[l.Handle] = wl[l.Handle] || l.Whitelisted
	}
	keys := make([]string, 0, len(handles))
	for i := 1; i <= len(handles); i++ {
		keys = append(keys, handles[fmt.Sprintf("P%d", i)])
	}
	// D7: with tools, Claude searches facts itself and sees only counts;
	// without (CLI), every fact goes inline and FitPrompt trims to 128k.
	var facts map[string][]string
	var counts map[string]int
	var err error
	if b.claude.SupportsTools() {
		counts, err = b.db.FactCounts(ctx, keys)
	} else {
		facts, err = b.db.PersonFacts(ctx, keys)
	}
	if err != nil {
		log.Printf("person facts: %v", err)
	}
	out := make([]router.Person, 0, len(keys))
	for i, k := range keys { // P1 is the requester (AssignHandles assigns it first)
		h := fmt.Sprintf("P%d", i+1)
		out = append(out, router.Person{Handle: h, Name: names[h], Whitelisted: wl[h], Facts: facts[k], FactCount: counts[k]})
	}
	return out
}

// factSearch gives Claude a search over the facts of people in this prompt
// only; handles are mapped back to person keys (D4, D7).
func (b *bot) factSearch(handles map[string]string) llm.FactSearch {
	byKey := map[string]string{}
	for h, k := range handles {
		byKey[k] = h
	}
	// label names a fact's owner: their handle if they are in this prompt,
	// else their name (D7: people from other chats are searchable too).
	label := func(ctx context.Context, key string) string {
		name, _ := b.db.PersonName(ctx, key)
		if h, ok := byKey[key]; ok {
			return h
		}
		if name == "" {
			return "someone #" + store.IDTag(key)
		}
		if taken, _ := b.db.NameTaken(ctx, name, key); taken {
			return name + " #" + store.IDTag(key) // two people share the name
		}
		return name
	}
	return func(ctx context.Context, person, query string) string {
		person = strings.TrimSpace(person)
		var (
			found []store.Fact
			keys  []string
			err   error
		)
		switch {
		case person == "":
			found, err = b.db.SearchAllFacts(ctx, query, 30)
		default:
			if k, ok := handles[strings.ToUpper(person)]; ok {
				keys = []string{k}
			} else {
				people, ferr := b.db.FindPeople(ctx, person, 5)
				if ferr != nil {
					err = ferr
					break
				}
				for _, p := range people {
					keys = append(keys, p.Key)
				}
				if len(keys) == 0 {
					return "no one known as " + person
				}
			}
			found, err = b.db.SearchPersonFacts(ctx, keys, query, 30)
		}
		if err != nil {
			log.Printf("search facts: %v", err)
			return "search failed"
		}
		log.Printf("search_facts person=%q query=%q -> %d", person, query, len(found))
		var sb strings.Builder
		if len(found) == 0 && len(keys) > 0 && strings.TrimSpace(query) != "" {
			// Compressed facts are labelled ("Origin: ...") and may not share
			// words with the query, so fall back to the newest facts (D7).
			if found, err = b.db.SearchPersonFacts(ctx, keys, "", 30); err != nil || len(found) == 0 {
				return "no stored facts about " + person
			}
			sb.WriteString("no exact match; newest facts instead:\n")
		} else if len(found) == 0 {
			return "no matching facts"
		}
		for _, f := range found {
			fmt.Fprintf(&sb, "%s: %s\n", label(ctx, f.Person), f.Text)
		}
		return sb.String()
	}
}

// applyTags saves what Claude asked to remember, enforcing who may change
// what: memories and person facts from anyone's request (B12, D4), forgets
// and persona changes only from whitelisted requesters (D2, D4).
func (b *bot) applyTags(ctx context.Context, chatKey string, req router.Line, handles map[string]string, r router.ParsedReply) {
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
	for _, f := range r.PersonFacts {
		key, who := resolve(f, true)
		if key == "" {
			log.Printf("person fact for unknown %s ignored", who)
			continue
		}
		if err := b.db.AddPersonFacts(ctx, key, chatKey, []string{f.Text}); err != nil {
			log.Printf("save person fact: %v", err)
		} else {
			log.Printf("saved fact for %s (%s)", who, key)
		}
	}
	for _, f := range r.Forgets {
		key, who := resolve(f, false)
		if key == "" || !req.Whitelisted {
			log.Printf("forget for %s ignored (requester whitelisted=%v)", who, req.Whitelisted)
			continue
		}
		if done, err := b.db.ForgetPersonFact(ctx, key, f.Text); err != nil {
			log.Printf("forget person fact: %v", err)
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
func (b *bot) send(_ context.Context, chat types.JID, text string, quote *events.Message) {
	// Own context: a long queue must not inherit the request's deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := b.pacer.Wait(ctx); err != nil {
		log.Printf("send to %s dropped: queue wait: %v", chat, err)
		return
	}
	msg := &waE2E.Message{ExtendedTextMessage: &waE2E.ExtendedTextMessage{Text: proto.String(text)}}
	if quote != nil {
		msg.ExtendedTextMessage.ContextInfo = &waE2E.ContextInfo{
			StanzaID:      proto.String(quote.Info.ID),
			Participant:   proto.String(quote.Info.Sender.ToNonAD().String()),
			QuotedMessage: quote.Message,
		}
	}
	if _, err := b.wa.SendMessage(ctx, chat, msg); err != nil {
		log.Printf("send to %s: %v", chat, err)
	}
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
		people = append(people, router.Person{Handle: h, Name: name, Facts: f})
	}
	out, err := b.curator.Reply(ctx, router.CuratorSystem, router.CuratorPrompt(people, lines), nil, nil)
	if err != nil {
		log.Printf("curator: %v", err)
		return
	}
	r := router.ParseCurator(out)
	saved := 0
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
		if err := b.db.AddPersonFacts(ctx, key, chat, []string{f.Text}); err == nil {
			saved++
		}
	}
	for _, a := range r.Aliases {
		key := handles[a.Handle]
		if key == "" {
			continue
		}
		if err := b.db.AddPersonFacts(ctx, key, chat, []string{"Alias: " + a.Text}); err == nil {
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
	log.Printf("curator: %s read %d messages, saved %d facts/aliases", chat, len(lines), saved)
}
