package main

// O1-O5: Claude Code tasks. The bot only keeps the queue and reports back;
// the laptop runner (cmd/claude-runner) polls it over SSH with the
// `wagoya task` subcommand and does the work.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"go.mau.fi/whatsmeow/types"
	"go.mau.fi/whatsmeow/types/events"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/memtools"
	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// runTasks expires stale tasks and announces new task statuses every 15 s.
func (b *bot) runTasks() {
	for {
		b.announceTasks()
		time.Sleep(15 * time.Second)
	}
}

func (b *bot) announceTasks() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if _, err := b.db.ExpireTasks(ctx); err != nil {
		log.Printf("tasks: expire: %v", err)
	}
	ts, err := b.db.Unannounced(ctx)
	if err != nil {
		log.Printf("tasks: %v", err)
		return
	}
	for _, t := range ts {
		// Marked first: a failed send must not repeat the message forever.
		if err := b.db.MarkAnnounced(ctx, t.ID); err != nil {
			log.Printf("tasks: mark #%d: %v", t.ID, err)
			continue
		}
		chat, err := types.ParseJID(t.Chat)
		if err != nil {
			log.Printf("tasks: #%d: bad chat %q", t.ID, t.Chat)
			continue
		}
		text := taskMessage(t, b.taskNote(ctx, t))
		out, stored, mentions := router.ExpandMentions(text, b.nameOf(ctx))
		sentID := b.sendMentions(ctx, chat, out, nil, mentionJIDs(mentions))
		log.Printf("tasks: announced #%d (%s) in %s", t.ID, t.Status, t.Chat)
		if err := b.db.AddMessage(ctx, t.Chat, router.Line{Time: time.Now(), SenderName: b.cfg.BotName, FromBot: true, Text: stored, ID: sentID,
			To: []string{t.RequesterName}}); err != nil { // K7
			log.Printf("tasks: store: %v", err)
		}
	}
}

// taskNote is the message for t (R2): the report, then Rin's outro, or
// for a request to allow (R4) the fixed template alone.
func (b *bot) taskNote(ctx context.Context, t store.Task) string {
	n := router.TaskNote{ID: t.ID, Owner: t.RequesterName, Repo: t.Repo, Task: t.Text, Status: t.Status, Branch: t.Branch,
		Question: t.Question, Result: t.Result, Approval: t.Approval}
	body := router.TaskBody(n)
	if n.Asks() {
		return body
	}
	outro := router.TaskOutro(n)
	if !b.overBudget(ctx) {
		persona, _ := b.db.Persona(ctx)
		var st llm.Stats
		out, err := b.curator.Reply(llm.WithStats(ctx, &st), router.TaskSystem(b.cfg.BotName, persona), router.TaskPrompt(n), nil, nil)
		b.account(fmt.Sprintf("task note #%d", t.ID), st)
		if out = strings.TrimSpace(router.ParseReply(out).Text); err == nil && out != "" {
			outro = out
		} else {
			log.Printf("tasks: outro #%d: %v; using the fixed one", t.ID, err)
		}
	}
	if t.Status == store.TaskNeedsInput && !strings.Contains(outro, "#"+strconv.FormatInt(t.ID, 10)) {
		outro += fmt.Sprintf(" _(task #%d)_", t.ID) // so the answer can name the task
	}
	if outro = strings.TrimSpace(outro); outro != "" {
		body += "\n\n" + outro
	}
	return body
}

// taskMessage puts the marker on top and tags the owner in a group (O5).
func taskMessage(t store.Task, note string) string {
	mark := memtools.MarkCompleted
	if t.Status == store.TaskNeedsInput || t.Status == store.TaskNeedsApproval || (t.Status == store.TaskRunning && t.Approval != "") {
		mark = memtools.MarkQuestion // O6; R4
	}
	note = strings.ReplaceAll(strings.TrimSpace(note), "@{", "@ {") // only the server pins tags
	for _, m := range []string{memtools.MarkInitiated, memtools.MarkCompleted, memtools.MarkQuestion} {
		note = strings.TrimSpace(strings.TrimPrefix(note, m))
	}
	if strings.HasSuffix(t.Chat, "@"+types.GroupServer) {
		note = "@{" + t.Requester + "} " + note
	}
	return mark + "\n" + note
}

// taskCommand is `wagoya task ...`, run by the laptop runner over SSH:
//
//	wagoya task claim [repo ...]   prints a Claim as JSON, or {} when idle
//	wagoya task status <id>        heartbeat; prints the task's status
//	wagoya task done <id>          reads a store.Report as JSON on stdin
//	wagoya task ask <id>           reads a live permission prompt on stdin (R4)
//	wagoya task answer <id>        prints allow, deny or pending (R4; also a heartbeat)
func taskCommand(args []string, stdin io.Reader, stdout io.Writer) error {
	if exe, err := os.Executable(); err == nil {
		os.Chdir(filepath.Dir(exe)) // SSH starts in $HOME; .env and data/ are next to the binary
	}
	if err := config.LoadDotEnv(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "data"
	}
	db, err := store.Open(filepath.Join(dir, "bot.db"), 1)
	if err != nil {
		return err
	}
	defer db.Close()
	return runTaskCommand(context.Background(), db, args, stdin, stdout)
}

func runTaskCommand(ctx context.Context, db *store.Store, args []string, stdin io.Reader, stdout io.Writer) error {
	if len(args) == 0 {
		return errors.New("usage: wagoya task claim [repo ...] | status <id> | done <id> | ask <id> | answer <id>")
	}
	id := func() (int64, error) {
		if len(args) < 2 {
			return 0, errors.New("missing task id")
		}
		return strconv.ParseInt(args[1], 10, 64)
	}
	enc := json.NewEncoder(stdout)
	switch args[0] {
	case "claim":
		c, ok, err := db.ClaimTask(ctx, args[1:])
		if err != nil {
			return err
		}
		if !ok {
			_, err = fmt.Fprintln(stdout, "{}")
			return err
		}
		return enc.Encode(c)
	case "status":
		n, err := id()
		if err != nil {
			return err
		}
		st, err := db.Heartbeat(ctx, n)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, st)
		return err
	case "done":
		n, err := id()
		if err != nil {
			return err
		}
		var r store.Report
		if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&r); err != nil {
			return fmt.Errorf("read report: %w", err)
		}
		if rs := []rune(r.Result); len(rs) > 20000 {
			r.Result = string(rs[:20000]) + "\n…(cut)"
		}
		return db.FinishTask(ctx, n, r)
	case "ask":
		n, err := id()
		if err != nil {
			return err
		}
		b, err := io.ReadAll(io.LimitReader(stdin, 8<<10))
		if err != nil {
			return err
		}
		return db.AskApproval(ctx, n, strings.TrimSpace(string(b)))
	case "answer":
		n, err := id()
		if err != nil {
			return err
		}
		a, err := db.TakeApproval(ctx, n)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, a)
		return err
	}
	return fmt.Errorf("unknown task command %q", args[0])
}

// approvalWord reads an owner's reply to a request to allow (R4): strict
// words only, ignoring case, punctuation and spaces.
func approvalWord(text string) (allow, ok bool) {
	w := strings.ToLower(strings.TrimFunc(strings.TrimSpace(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}))
	switch w {
	case "yes", "y", "ok", "okay", "allow":
		return true, true
	case "no", "n", "deny":
		return false, true
	}
	return false, false
}

// handleApproval answers a request to allow from the owner's message, if
// one is waiting in this chat (R4). It returns true when the message was
// handled here and must not go to the model: a yes or no, or a reply to
// the request that is neither, which is asked again.
func (b *bot) handleApproval(ctx context.Context, chat types.JID, e *events.Message, key, text, quoted string) bool {
	t, ok, err := b.db.PendingApproval(ctx, chat.String(), key)
	if err != nil || !ok {
		return false
	}
	allow, isWord := approvalWord(text)
	var reply string
	switch {
	case isWord:
		if err := b.db.AnswerApproval(ctx, t.ID, allow); err != nil {
			log.Printf("tasks: approval #%d: %v", t.ID, err)
			return false
		}
		log.Printf("tasks: approval #%d: allow=%v", t.ID, allow)
		switch {
		case allow && t.Status == store.TaskRunning:
			reply = fmt.Sprintf("Allowed. Claude Code carries on with #%d.", t.ID)
		case allow:
			reply = fmt.Sprintf("Allowed. #%d continues once the laptop picks it up.", t.ID)
		case t.Status == store.TaskRunning:
			reply = fmt.Sprintf("Denied. Claude Code carries on with #%d without it.", t.ID)
		default:
			reply = fmt.Sprintf("Denied. #%d stays as it is.", t.ID)
		}
	case strings.Contains(quoted, router.ApprovalReply) || strings.Contains(quoted, "Reply *yes* to allow these"):
		reply = fmt.Sprintf("Task #%d is waiting on a yes or a no. %s", t.ID, router.ApprovalReply)
	default:
		return false
	}
	sentID := b.send(ctx, chat, reply, e)
	if err := b.db.AddMessage(ctx, chat.String(), router.Line{Time: time.Now(), SenderName: b.cfg.BotName, FromBot: true, Text: reply, ID: sentID,
		To: []string{t.RequesterName}}); err != nil {
		log.Printf("tasks: store: %v", err)
	}
	return true
}
