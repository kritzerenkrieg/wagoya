package memtools

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/router"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

var wib = time.FixedZone("WIB", 7*3600)

// Thursday 2026-11-05 14:24 WIB.
var clock = time.Date(2026, 11, 5, 14, 24, 0, 0, wib)

func schedTurn(t *testing.T, db *store.Store, req router.Line, chat string, hasOwner bool) func(tool, input string) string {
	t.Helper()
	handles := router.AssignHandles(nil, &req)
	tr := &Turn{DB: db, Chat: chat, Handles: handles, Requester: req, ChatHasOwner: hasOwner,
		Now: func() time.Time { return clock },
		ResolveChat: func(_ context.Context, name string) (string, string, error) {
			if name == "Futsal" {
				return "futsal@g.us", "Futsal", nil
			}
			return "", "", errors.New("no group of yours is called " + name)
		}}
	tools := tr.ScheduleTools()
	return func(tool, input string) string {
		for _, tl := range tools {
			if tl.Name == tool {
				return tl.Run(context.Background(), json.RawMessage(input))
			}
		}
		t.Fatalf("no tool %s", tool)
		return ""
	}
}

func openDB(t *testing.T) *store.Store {
	db, err := store.Open(filepath.Join(t.TempDir(), "s.db"), 16)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestScheduleRights(t *testing.T) {
	db := openDB(t)
	owner := schedTurn(t, db, john, "dm-john", true)
	other := schedTurn(t, db, jim, "group@g.us", true)
	strangerDM := schedTurn(t, db, jim, "dm-jim", false)
	for _, c := range []struct {
		call  func(string, string) string
		input string
		want  string
	}{
		{owner, `{"text":"eat lunch","at":"2026-11-06 12:00","repeat":"daily"}`, "scheduled: job 1, Fri 2026-11-06 12:00 in this chat, repeating daily"},
		{owner, `{"text":"futsal tonight","at":"2026-11-06 18:00","chat":"Futsal"}`, "in Futsal"},
		{owner, `{"text":"x","at":"2026-11-06 18:00","chat":"Chess"}`, "no group of yours"},
		{owner, `{"text":"x","at":"2026-11-05 14:00"}`, "has passed"},
		{owner, `{"text":"x","at":"tomorrow noon"}`, "must look like"},
		{owner, `{"text":"x","in_minutes":5,"repeat":"minutely"}`, "repeat must be"},
		{owner, `{"text":"x","at":"2027-12-01 10:00"}`, "a year ahead"},
		{other, `{"text":"call mom","in_minutes":30}`, "scheduled: job"},
		{other, `{"text":"call mom","in_minutes":30,"repeat":"daily"}`, "only owners can schedule repeating"},
		{other, `{"text":"x","in_minutes":30,"chat":"Futsal"}`, "only owners can schedule into another chat"},
		{other, `{"text":"go sleep","at":"2026-11-05 23:00","self_initiated":true,"repeat":"daily"}`, "scheduled"},
		{strangerDM, `{"text":"go sleep","in_minutes":60,"self_initiated":true}`, "only schedule your own messages in chats with one of your owners"},
		{strangerDM, `{"text":"call mom","in_minutes":60}`, "scheduled"},
	} {
		if got := c.call("schedule_message", c.input); !strings.Contains(got, c.want) {
			t.Errorf("%s: got %q, want %q", c.input, got, c.want)
		}
	}
	// Weekdays starting on a Saturday move to Monday.
	if got := owner("schedule_message", `{"text":"standup","at":"2026-11-07 09:00","repeat":"weekdays"}`); !strings.Contains(got, "Mon 2026-11-09 09:00") {
		t.Errorf("weekdays: %q", got)
	}
}

func TestScheduleLimitListCancel(t *testing.T) {
	db := openDB(t)
	owner := schedTurn(t, db, john, "g@g.us", true)
	other := schedTurn(t, db, jim, "g@g.us", true)
	for i := 0; i < MaxJobsPerChat; i++ {
		if got := other("schedule_message", `{"text":"r","in_minutes":10}`); !strings.HasPrefix(got, "scheduled") {
			t.Fatalf("job %d: %q", i, got)
		}
	}
	if got := owner("schedule_message", `{"text":"one more","in_minutes":10}`); !strings.Contains(got, "already has 10") {
		t.Fatalf("limit: %q", got)
	}
	list := other("list_scheduled", `{}`)
	if strings.Count(list, "job ") != MaxJobsPerChat {
		t.Fatalf("list: %q", list)
	}
	id := regexp.MustCompile(`job (\d+)`).FindStringSubmatch(list)[1]
	// A self-initiated job for Jim: he can cancel it; a stranger can't cancel Jim's.
	mary := schedTurn(t, db, router.Line{SenderID: "mary", SenderName: "Mary"}, "g@g.us", true)
	if got := mary("cancel_scheduled", `{"id":`+id+`}`); !strings.Contains(got, "only owners") {
		t.Fatalf("mary cancelled jim's: %q", got)
	}
	if got := other("cancel_scheduled", `{"id":`+id+`}`); got != "cancelled" {
		t.Fatalf("own cancel: %q", got)
	}
	if got := owner("cancel_scheduled", `{"id":`+regexp.MustCompile(`job (\d+)`).FindStringSubmatch(other("list_scheduled", `{}`))[1]+`}`); got != "cancelled" {
		t.Fatalf("owner cancel: %q", got)
	}
	if got := schedTurn(t, db, jim, "elsewhere", true)("cancel_scheduled", `{"id":999}`); !strings.Contains(got, "no job") {
		t.Fatalf("missing: %q", got)
	}
}

func TestDueAction(t *testing.T) {
	at := clock
	// One-off: sent when under 12 h late, dropped after.
	if send, next := DueAction(store.Job{NextAt: at}, at.Add(time.Hour), 12*time.Hour, 30*time.Minute); !send || !next.IsZero() {
		t.Fatal("one-off on time")
	}
	if send, _ := DueAction(store.Job{NextAt: at}, at.Add(13*time.Hour), 12*time.Hour, 30*time.Minute); send {
		t.Fatal("stale one-off sent")
	}
	// Daily after 3 days of downtime: not sent, next run is the next 14:24.
	send, next := DueAction(store.Job{NextAt: at, Repeat: "daily"}, at.Add(72*time.Hour+time.Minute), 12*time.Hour, 30*time.Minute)
	if send || !next.Equal(at.AddDate(0, 0, 4)) {
		t.Fatalf("missed daily: send=%v next=%v", send, next)
	}
	// Weekdays from Friday goes to Monday.
	fri := time.Date(2026, 11, 6, 9, 0, 0, 0, wib)
	if send, next := DueAction(store.Job{NextAt: fri, Repeat: "weekdays"}, fri.Add(time.Minute), time.Hour, 30*time.Minute); !send || next.Weekday() != time.Monday {
		t.Fatalf("weekdays: %v %v", send, next)
	}
}

// L10: "@P1" in a scheduled message is stored as the person's key, so it
// still tags them when it is sent, and lists show their name.
func TestSchedulePinsMentions(t *testing.T) {
	db := openDB(t)
	db.UpsertPerson(context.Background(), "john", "john", "", "John", clock)
	run := schedTurn(t, db, john, "g@g.us", true) // John is P1 in this turn
	if got := run("schedule_message", `{"text":"@P1 futsal in 15 minutes. @P9 too","in_minutes":30}`); !strings.HasPrefix(got, "scheduled") {
		t.Fatalf("schedule: %q", got)
	}
	jobs, _ := db.ListJobs(context.Background(), "g@g.us", "")
	if len(jobs) != 1 || jobs[0].Text != "@{john} futsal in 15 minutes. @P9 too" {
		t.Fatalf("stored: %+v", jobs)
	}
	if got := run("list_scheduled", `{}`); !strings.Contains(got, "| @John futsal in 15 minutes.") {
		t.Fatalf("list: %q", got)
	}
}
