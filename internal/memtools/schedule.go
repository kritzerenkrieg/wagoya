package memtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// Scheduling tools (J7, J9, J10): schedule_message, list_scheduled and
// cancel_scheduled.

const (
	MaxJobsPerChat = 10
	maxJobText     = 500
	minLead        = 30 * time.Second
)

// Repeats a job may have; the most frequent is hourly (J10).
var Repeats = []string{"none", "hourly", "daily", "weekdays", "weekly"}

// NextRun is the run after t for a repeat, or zero for a one-off.
func NextRun(t time.Time, repeat string) time.Time {
	switch repeat {
	case "hourly":
		return t.Add(time.Hour)
	case "daily":
		return t.AddDate(0, 0, 1)
	case "weekly":
		return t.AddDate(0, 0, 7)
	case "weekdays":
		return weekday(t.AddDate(0, 0, 1))
	}
	return time.Time{}
}

// DueAction decides what to do with a due job: whether to send it (not too
// late), and for a recurring job its next run after now (missed runs are
// skipped). next is zero for a one-off, which is then deleted.
func DueAction(j store.Job, now time.Time, oneOffGrace, repeatGrace time.Duration) (send bool, next time.Time) {
	late := now.Sub(j.NextAt)
	if j.Repeat == "" {
		return late <= oneOffGrace, time.Time{}
	}
	next = j.NextAt
	for !next.After(now) {
		next = NextRun(next, j.Repeat)
	}
	return late <= repeatGrace, next
}

// weekday moves t forward to Monday if it falls on a weekend.
func weekday(t time.Time) time.Time {
	for t.Weekday() == time.Saturday || t.Weekday() == time.Sunday {
		t = t.AddDate(0, 0, 1)
	}
	return t
}

func (t *Turn) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

// ScheduleTools returns the scheduling tools for a reply.
func (t *Turn) ScheduleTools() []llm.Tool {
	return []llm.Tool{t.scheduleTool(), t.listTool(), t.cancelTool()}
}

func (t *Turn) scheduleTool() llm.Tool {
	return llm.Tool{
		Name: "schedule_message",
		Description: "Schedule a message you will send later: a reminder someone asked for, or your own idea (self_initiated), like nagging them to eat or sleep. " +
			"Write text as the exact message you will send then, in character. Times are local, like <now>. " +
			"Owners (whitelisted) may repeat it and target another chat they are in; others only get one-off reminders for themselves in this chat. " +
			"At most 10 pending per chat. Only say it is scheduled if this returned scheduled.",
		Schema: map[string]any{"type": "object", "required": []string{"text"}, "properties": map[string]any{
			"text":           map[string]any{"type": "string", "description": "The message to send, under 500 characters."},
			"at":             map[string]any{"type": "string", "description": "Local date and time, \"YYYY-MM-DD HH:MM\". Give at or in_minutes."},
			"in_minutes":     map[string]any{"type": "integer", "description": "Alternative to at: minutes from now."},
			"repeat":         map[string]any{"type": "string", "enum": Repeats, "description": "Default none."},
			"chat":           map[string]any{"type": "string", "description": "Optional, owners only: a group name, or \"dm\" for the requester's private chat. Default: this chat."},
			"self_initiated": map[string]any{"type": "boolean", "description": "True if it is your own idea, not something asked for."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				Text          string `json:"text"`
				At            string `json:"at"`
				InMinutes     int    `json:"in_minutes"`
				Repeat        string `json:"repeat"`
				Chat          string `json:"chat"`
				SelfInitiated bool   `json:"self_initiated"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			out, err := t.schedule(ctx, in.Text, in.At, in.InMinutes, in.Repeat, in.Chat, in.SelfInitiated)
			if err != nil {
				log.Printf("schedule_message rejected: %v", err)
				return errText(err)
			}
			return out
		},
	}
}

func (t *Turn) schedule(ctx context.Context, text, at string, inMinutes int, repeat, chat string, self bool) (string, error) {
	text = strings.TrimSpace(text)
	repeat = strings.ToLower(strings.TrimSpace(repeat))
	if repeat == "" {
		repeat = "none"
	}
	now := t.now()
	switch {
	case text == "":
		return "", errors.New("text is empty")
	case utf8.RuneCountInString(text) > maxJobText:
		return "", fmt.Errorf("text is too long (max %d characters)", maxJobText)
	case !slices.Contains(Repeats, repeat):
		return "", fmt.Errorf("repeat must be one of %v", Repeats)
	}
	var when time.Time
	switch {
	case strings.TrimSpace(at) != "":
		w, err := time.ParseInLocation("2006-01-02 15:04", strings.TrimSpace(at), now.Location())
		if err != nil {
			return "", errors.New(`at must look like "2006-01-02 15:04" (local time)`)
		}
		when = w
	case inMinutes > 0:
		when = now.Add(time.Duration(inMinutes) * time.Minute)
	default:
		return "", errors.New("give at or in_minutes")
	}
	if repeat == "weekdays" {
		when = weekday(when)
	}
	switch {
	case when.Before(now.Add(minLead)):
		return "", fmt.Errorf("that time has passed or is too close; it is now %s", now.Format("2006-01-02 15:04"))
	case when.After(now.AddDate(1, 0, 0)):
		return "", errors.New("at most a year ahead")
	}
	owner := t.Requester.Whitelisted
	target, where := t.Chat, "this chat"
	if c := strings.TrimSpace(chat); c != "" && !strings.EqualFold(c, "this") && !strings.EqualFold(c, "this chat") {
		if !owner || t.ResolveChat == nil {
			return "", errors.New("only owners can schedule into another chat")
		}
		var err error
		if target, where, err = t.ResolveChat(ctx, c); err != nil {
			return "", err
		}
	}
	// J10: who may schedule what.
	switch {
	case owner:
	case self:
		if !t.ChatHasOwner {
			return "", errors.New("you can only schedule your own messages in chats with one of your owners")
		}
	case repeat != "none":
		return "", errors.New("only owners can schedule repeating messages; offer a one-off reminder instead")
	}
	n, err := t.DB.CountJobs(ctx, target)
	if err != nil {
		return "", errors.New("storage failed")
	}
	if n >= MaxJobsPerChat {
		return "", fmt.Errorf("that chat already has %d scheduled messages; cancel one first", n)
	}
	if repeat == "none" {
		repeat = ""
	}
	id, err := t.DB.AddJob(ctx, store.Job{Chat: target, CreatedBy: t.reqKey(), CreatedName: t.Requester.SenderName,
		SelfInitiated: self, Text: text, NextAt: when, Repeat: repeat})
	if err != nil {
		return "", errors.New("storage failed")
	}
	log.Printf("scheduled job %d in %s at %s repeat=%q self=%v by %s", id, target, when.Format(time.RFC3339), repeat, self, t.reqKey())
	every := ""
	if repeat != "" {
		every = ", repeating " + repeat
	}
	return fmt.Sprintf("scheduled: job %d, %s in %s%s", id, when.Format("Mon 2006-01-02 15:04"), where, every), nil
}

func (t *Turn) listTool() llm.Tool {
	return llm.Tool{
		Name:        "list_scheduled",
		Description: "List the messages scheduled in this chat, and the requester's own scheduled messages elsewhere, with their job ids.",
		Schema:      map[string]any{"type": "object", "properties": map[string]any{}},
		Run: func(ctx context.Context, _ json.RawMessage) string {
			jobs, err := t.DB.ListJobs(ctx, t.Chat, t.reqKey())
			if err != nil {
				return "error: storage failed"
			}
			if len(jobs) == 0 {
				return "nothing scheduled"
			}
			var b strings.Builder
			for _, j := range jobs {
				rep, where, who := "once", "this chat", j.CreatedName
				if j.Repeat != "" {
					rep = j.Repeat
				}
				if j.Chat != t.Chat {
					where = "another chat"
				}
				if j.SelfInitiated {
					who = "your own idea, for " + who
				}
				fmt.Fprintf(&b, "job %d | %s | %s | %s | by %s | %s\n", j.ID, j.NextAt.In(t.now().Location()).Format("Mon 2006-01-02 15:04"), rep, where, who, j.Text)
			}
			return b.String()
		},
	}
}

func (t *Turn) cancelTool() llm.Tool {
	return llm.Tool{
		Name:        "cancel_scheduled",
		Description: "Cancel a scheduled message by its job id from list_scheduled. People can cancel their own; owners can cancel any.",
		Schema: map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{
			"id": map[string]any{"type": "integer"},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				ID int64 `json:"id"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			j, ok, err := t.DB.JobByID(ctx, in.ID)
			switch {
			case err != nil:
				return "error: storage failed"
			case !ok || (j.Chat != t.Chat && j.CreatedBy != t.reqKey()):
				return fmt.Sprintf("error: no job %d here; use list_scheduled", in.ID)
			case !t.Requester.Whitelisted && (j.CreatedBy == "" || j.CreatedBy != t.reqKey()):
				return "error: only owners can cancel someone else's scheduled message"
			}
			if err := t.DB.DeleteJob(ctx, in.ID); err != nil {
				return "error: storage failed"
			}
			log.Printf("cancelled job %d by %s", in.ID, t.reqKey())
			return "cancelled"
		},
	}
}
