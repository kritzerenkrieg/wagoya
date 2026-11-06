// Package memtools gives the model its memory as tools (J3): search_facts,
// save_fact, invalidate_fact and save_memory. Each call is checked, gets its
// provenance from the bot (J1, J4), not from the model, and reports errors
// back to the model instead of failing silently.
package memtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// Turn is the context of one model call: who asked, in which chat, and who
// is shown with which handle.
type Turn struct {
	DB        *store.Store
	Chat      string
	Handles   map[string]string // "P1" -> person key
	Names     map[string]string // "P1" -> display name, for "@Full Name" tags in scheduled text (L10)
	Requester router.Line       // its Handle and Whitelisted matter
	Spoke     map[string]bool   // handles with a message in this prompt
	Curator   bool              // a curator pass (H1): everything it saves is inferred (J4)
	Vectors   *Vectors          // J2 c: embedding search; nil means FTS only
	AfterSave func()            // called after a fact is saved (wakes the embedder)

	// Scheduling (J7, J10).
	ChatHasOwner bool                                                                   // a whitelisted person is in this chat
	ResolveChat  func(ctx context.Context, name string) (chat, label string, err error) // owners: another chat by name, or "dm"
	Now          func() time.Time                                                       // clock; nil means time.Now

	saved atomic.Int32
}

// Saved is how many facts and memories this turn saved.
func (t *Turn) Saved() int { return int(t.saved.Load()) }

func (t *Turn) reqKey() string { return t.Handles[t.Requester.Handle] }

// Tools returns the memory tools for a reply.
func (t *Turn) Tools() []llm.Tool {
	return []llm.Tool{llm.FactSearchTool(t.Search()), t.saveFactTool(), t.invalidateTool(), t.saveMemoryTool(), t.chatHistoryTool()}
}

// CuratorTools returns the tools for a curator pass: save_fact only.
func (t *Turn) CuratorTools() []llm.Tool {
	t.Curator = true
	return []llm.Tool{t.saveFactTool()}
}

// label names a fact's owner for the model: their handle if they are in the
// prompt, else their name, with an ID tag if the name is shared (D7).
func (t *Turn) label(ctx context.Context, key string) string {
	for h, k := range t.Handles {
		if k == key {
			return h
		}
	}
	name, _ := t.DB.PersonName(ctx, key)
	if name == "" {
		return "someone #" + store.IDTag(key)
	}
	if taken, _ := t.DB.NameTaken(ctx, name, key); taken {
		return name + " #" + store.IDTag(key)
	}
	return name
}

// keysFor resolves a handle or a name to profile keys.
func (t *Turn) keysFor(ctx context.Context, person string) ([]string, error) {
	if k, ok := t.Handles[strings.ToUpper(person)]; ok {
		return []string{k}, nil
	}
	people, err := t.DB.FindPeople(ctx, person, 5)
	var keys []string
	for _, p := range people {
		keys = append(keys, p.Key)
	}
	return keys, err
}

// Search is search_facts (D7): any known person, from any chat. Results carry
// fact ids for invalidate_fact.
func (t *Turn) Search() llm.FactSearch {
	return func(ctx context.Context, person, query string, history bool) string {
		person = strings.TrimSpace(person)
		var (
			found []store.Fact
			keys  []string
			err   error
		)
		// J2 c: embed the query while FTS runs.
		var qvec []float32
		embedded := make(chan struct{})
		if t.Vectors != nil && strings.TrimSpace(query) != "" {
			go func() {
				defer close(embedded)
				v, err := t.Vectors.Vec(ctx, query)
				if err != nil {
					log.Printf("search facts: embed query: %v (FTS only)", err)
				}
				qvec = v
			}()
		} else {
			close(embedded)
		}
		if person == "" {
			found, err = t.DB.SearchAllFacts(ctx, query, 30, history)
		} else {
			if keys, err = t.keysFor(ctx, person); err == nil {
				if len(keys) == 0 {
					<-embedded
					return "no one known as " + person
				}
				found, err = t.DB.SearchPersonFacts(ctx, keys, query, 30, history)
			}
		}
		<-embedded
		if err != nil {
			log.Printf("search facts: %v", err)
			return "search failed"
		}
		nvec := 0
		if qvec != nil {
			var scope []string
			if person != "" {
				scope = keys
			}
			hits, verr := t.DB.VectorSearch(ctx, t.Vectors.E.Model(), qvec, scope, 20, history)
			if verr != nil {
				log.Printf("search facts: vectors: %v", verr)
			}
			nvec = len(hits)
			found = hybrid(hits, found, 30)
		}
		log.Printf("search_facts person=%q query=%q history=%v -> %d (%d by vector)", person, query, history, len(found), nvec)
		var sb strings.Builder
		if len(found) == 0 && len(keys) > 0 && strings.TrimSpace(query) != "" {
			// Compressed facts are labelled ("Origin: ...") and may not share
			// words with the query, so fall back to the newest facts (D7).
			if found, err = t.DB.SearchPersonFacts(ctx, keys, "", 30, history); err != nil || len(found) == 0 {
				return "no stored facts about " + person
			}
			sb.WriteString("no exact match; newest facts instead:\n")
		} else if len(found) == 0 {
			return "no matching facts"
		}
		for _, f := range found {
			by := "" // J4: hearsay names its source
			if f.Tier == store.TierParticipant && f.SaidBy != "" && f.SaidBy != f.Person {
				by = " (said by " + t.label(ctx, f.SaidBy) + ")"
			}
			fmt.Fprintf(&sb, "fact %d | %s: %s%s\n", f.ID, t.label(ctx, f.Person), f.String(), by)
		}
		return sb.String()
	}
}

// RequestTier is the tier of something the requester told the bot (J4):
// owner if whitelisted, self if it is about themselves, else hearsay.
func RequestTier(req router.Line, reqKey, subject string) store.Tier {
	switch {
	case req.Whitelisted:
		return store.TierOwner
	case reqKey != "" && subject == reqKey:
		return store.TierSelf
	}
	return store.TierParticipant
}

var (
	labelRe   = regexp.MustCompile(`^[\p{L}][\p{L}\p{N} /&'-]{0,23}: \S`)
	handleRe  = regexp.MustCompile(`^P\d+$`)
	kinds     = []string{"fact", "state", "claim", "joke"}
	sources   = []string{"requester", "self", "inferred"}
	maxFact   = 160
	maxMemory = 240
)

func errText(err error) string { return "error: " + err.Error() }

func (t *Turn) saveFactTool() llm.Tool {
	props := map[string]any{
		"subject": map[string]any{"type": "string", "description": "Who the fact is about: a handle like P2, or a name (e.g. Omar, or Omar #9249 when search showed an ID tag) for someone not in this chat. A new name gets a new profile."},
		"content": map[string]any{"type": "string", "description": "The fact, compressed: a one-word label and under 12 words, e.g. \"Job: nurse at Acme\", \"Likes: board games, jazz\", \"Alias: Yan\". In English."},
		"kind":    map[string]any{"type": "string", "enum": kinds, "description": "fact (default); state for something that passes within a day (on the way home, sick today); claim for something said about someone that you can't verify; joke for banter."},
	}
	desc := "Save a durable fact about a person to your global memory: who they are, job, studies, origin, relationships, likes, habits, things they asked you to remember. Skip what is already known, small talk and anything sensitive (passwords, phone numbers, addresses, health, money). Returns saved, or an error to fix."
	if !t.Curator {
		props["source"] = map[string]any{"type": "string", "enum": sources, "description": "Who said it: requester (default) if the person asking you told you; self if the subject said it about themselves in the transcript; inferred if you are reading between the lines."}
		desc += " Only say you saved something if this returned saved."
	}
	return llm.Tool{
		Name: "save_fact", Description: desc,
		Schema: map[string]any{"type": "object", "properties": props, "required": []string{"subject", "content"}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct{ Subject, Content, Kind, Source string }
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			out, err := t.saveFact(ctx, in.Subject, in.Content, in.Kind, in.Source)
			if err != nil {
				log.Printf("save_fact rejected: %v", err)
				return errText(err)
			}
			return out
		},
	}
}

func (t *Turn) saveFact(ctx context.Context, subject, content, kind, source string) (string, error) {
	subject = strings.TrimSpace(subject)
	content = strings.Join(strings.Fields(content), " ")
	kind, source = strings.ToLower(strings.TrimSpace(kind)), strings.ToLower(strings.TrimSpace(source))
	switch {
	case subject == "":
		return "", errors.New("subject is empty")
	case !labelRe.MatchString(content):
		return "", errors.New(`content must start with a short label and a colon, e.g. "Job: nurse"`)
	case utf8.RuneCountInString(content) > maxFact:
		return "", fmt.Errorf("content is too long (max %d characters); compress it", maxFact)
	case kind != "" && !slices.Contains(kinds, kind):
		return "", fmt.Errorf("kind must be one of %v", kinds)
	case source != "" && !slices.Contains(sources, source):
		return "", fmt.Errorf("source must be one of %v", sources)
	case router.MergesIdentities(content):
		return "", errors.New("not saved: never record that two people are the same person, interchangeable or alts; each person is their own account")
	}
	var key, who string
	if h := strings.ToUpper(subject); handleRe.MatchString(h) {
		if key = t.Handles[h]; key == "" {
			return "", fmt.Errorf("no participant has the handle %s", h)
		}
		who = h
	} else {
		p, err := t.DB.PersonByName(ctx, subject, time.Now())
		if err != nil || p.Key == "" {
			return "", fmt.Errorf("cannot make a profile for %q", subject)
		}
		key, who = p.Key, subject
	}
	src := store.Source{Chat: t.Chat, Kind: store.Kind(kind)}
	switch {
	case t.Curator || source == "inferred":
		src.Tier = store.TierInferred
	case source == "self":
		if !handleRe.MatchString(who) || !t.Spoke[who] {
			return "", errors.New("source self needs the subject to have said it in this transcript; use requester or inferred")
		}
		src.Tier, src.SaidBy = store.TierSelf, key
	default:
		src.Tier, src.SaidBy = RequestTier(t.Requester, t.reqKey(), key), t.reqKey()
	}
	if err := t.DB.AddPersonFacts(ctx, key, src, []string{content}); err != nil {
		log.Printf("save fact: %v", err)
		return "", errors.New("storage failed")
	}
	t.saved.Add(1)
	log.Printf("saved fact for %s (%s, %s)", who, key, src.Tier)
	if t.AfterSave != nil {
		t.AfterSave()
	}
	// H3: a name-only profile under this nickname is this person.
	if label, nick, _ := strings.Cut(content, ":"); strings.EqualFold(strings.TrimSpace(label), "alias") {
		from := "name:" + strings.ToLower(strings.Join(strings.Fields(nick), " "))
		if ok, err := t.DB.MergePerson(ctx, from, key); err != nil {
			log.Printf("merge %s: %v", from, err)
		} else if ok {
			log.Printf("merged %s into %s", from, key)
		}
	}
	return "saved", nil
}

func (t *Turn) invalidateTool() llm.Tool {
	return llm.Tool{
		Name: "invalidate_fact",
		Description: "Mark a stored fact as no longer true, by its id from search_facts. It is kept as history, hidden from normal search. " +
			"Only when the request has whitelisted=\"yes\" and asks to forget or correct something, or when the person the fact is about says it is no longer true. " +
			"For a correction, also save the new fact. Only say you forgot something if this returned done.",
		Schema: map[string]any{"type": "object", "required": []string{"id", "reason"}, "properties": map[string]any{
			"id":     map[string]any{"type": "integer", "description": "The fact id from search_facts (\"fact 123 | ...\")."},
			"reason": map[string]any{"type": "string", "description": "Short reason, e.g. \"moved to Bali\" or \"asked to forget\"."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				ID     int64  `json:"id"`
				Reason string `json:"reason"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			out, err := t.invalidate(ctx, in.ID, in.Reason)
			if err != nil {
				log.Printf("invalidate_fact %d rejected: %v", in.ID, err)
				return errText(err)
			}
			return out
		},
	}
}

func (t *Turn) invalidate(ctx context.Context, id int64, reason string) (string, error) {
	reason = strings.Join(strings.Fields(reason), " ")
	if reason == "" {
		return "", errors.New("give a short reason")
	}
	if utf8.RuneCountInString(reason) > 120 {
		reason = string([]rune(reason)[:120])
	}
	f, ok, err := t.DB.FactByID(ctx, id)
	if err != nil {
		return "", errors.New("storage failed")
	}
	if !ok {
		return "", fmt.Errorf("no fact %d; search first and use an id from the results", id)
	}
	if !f.Valid() {
		return "already outdated", nil
	}
	var by store.Tier
	switch {
	case t.Requester.Whitelisted:
		by = store.TierOwner
	case f.Person == t.reqKey() && t.reqKey() != "":
		by = store.TierSelf
	default:
		return "", errors.New("only owners, or the person the fact is about, can retract it")
	}
	done, err := t.DB.InvalidateFactID(ctx, id, by, reason+" ("+t.Requester.SenderName+")")
	switch {
	case errors.Is(err, store.ErrOutranked):
		return "", errors.New("that fact comes from a more trusted source; only an owner can retract it")
	case err != nil:
		return "", errors.New("storage failed")
	case !done:
		return "already outdated", nil
	}
	t.saved.Add(1)
	log.Printf("invalidated fact %d of %s (%s)", id, f.Person, by)
	return "done", nil
}

func (t *Turn) saveMemoryTool() llm.Tool {
	return llm.Tool{
		Name: "save_memory",
		Description: "Save a durable fact about this chat (not about one person): a decision, plan, preference or shared fact, " +
			"e.g. \"Futsal every Friday 8pm\". Skip small talk and what is already in <memories>.",
		Schema: map[string]any{"type": "object", "required": []string{"category", "content"}, "properties": map[string]any{
			"category": map[string]any{"type": "string", "enum": router.Categories},
			"content":  map[string]any{"type": "string", "description": "Short, under 25 words."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct{ Category, Content string }
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			cat, text := strings.ToLower(strings.TrimSpace(in.Category)), strings.Join(strings.Fields(in.Content), " ")
			switch {
			case !slices.Contains(router.Categories, cat):
				return fmt.Sprintf("error: category must be one of %v", router.Categories)
			case utf8.RuneCountInString(text) < 3:
				return "error: content is empty"
			case utf8.RuneCountInString(text) > maxMemory:
				return fmt.Sprintf("error: content is too long (max %d characters)", maxMemory)
			case router.MergesIdentities(text):
				return "error: not saved: never record that two people are the same person, interchangeable or alts"
			}
			if err := t.DB.AddMemories(ctx, t.Chat, []router.Memory{{Category: cat, Text: text}}); err != nil {
				log.Printf("save memory: %v", err)
				return "error: storage failed"
			}
			t.saved.Add(1)
			return "saved"
		},
	}
}

// historyPage is how many older messages chat_history returns per call (L9).
const historyPage = 20

// chatHistoryTool searches this chat's messages from before <transcript>,
// kept for store.MessageTTL (L9). Only this chat: never another chat's.
func (t *Turn) chatHistoryTool() llm.Tool {
	return llm.Tool{
		Name: "chat_history",
		Description: "Search this chat's older messages, from before <transcript> (kept for the last 3 days only). " +
			"Use it when someone refers to something said earlier that isn't in <transcript> or <summary>. " +
			"query: words or a name to look for; empty lists the messages just before <transcript>. page: 0 for the first results, 1 for the next.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"query": map[string]any{"type": "string"},
			"page":  map[string]any{"type": "integer", "minimum": 0},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				Query string
				Page  int
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			if in.Page < 0 || in.Page > 50 {
				return "error: page must be 0 to 50"
			}
			lines, err := t.DB.OlderMessages(ctx, t.Chat, in.Query, historyPage, in.Page*historyPage)
			if err != nil {
				log.Printf("chat history: %v", err)
				return "error: storage failed"
			}
			if len(lines) == 0 {
				if in.Page > 0 {
					return "no more older messages"
				}
				return "no older messages match (only the last 3 days are kept)"
			}
			var b strings.Builder
			fmt.Fprintf(&b, "%d older messages from this chat, oldest first:\n", len(lines))
			for _, l := range lines {
				who := l.SenderName
				if l.FromBot {
					who += " (you)"
				}
				text := strings.Join(strings.Fields(l.Text), " ")
				if r := []rune(text); len(r) > 500 {
					text = string(r[:500]) + "…"
				}
				fmt.Fprintf(&b, "[%s] %s: %s\n", l.Time.Format("2006-01-02 15:04"), who, text)
			}
			if len(lines) == historyPage {
				fmt.Fprintf(&b, "(page %d may have more)", in.Page+1)
			}
			return b.String()
		},
	}
}
