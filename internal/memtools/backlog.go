package memtools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// Backlog tools (U1): add_task, update_task and list_tasks. Every call works
// on the requester's own tasks only; another person's task looks the same
// as one that doesn't exist.

const (
	maxTaskName   = 200
	maxTaskDesc   = 2000
	maxTaskList   = 30
	taskTimeInput = "2006-01-02 15:04"
)

// BacklogTools returns the backlog tools for a reply. Without a requester
// key (nobody to own the tasks) there are none.
func (t *Turn) BacklogTools() []llm.Tool {
	if t.reqKey() == "" {
		return nil
	}
	return []llm.Tool{t.addTaskTool(), t.updateTaskTool(), t.listTasksTool()}
}

func (t *Turn) parseDue(s string) (time.Time, error) {
	w, err := time.ParseInLocation(taskTimeInput, strings.TrimSpace(s), t.now().Location())
	if err != nil {
		return time.Time{}, errors.New(`due must look like "2006-01-02 15:04" (local time)`)
	}
	return w, nil
}

func checkTaskText(name, desc string) error {
	switch {
	case utf8.RuneCountInString(name) > maxTaskName:
		return fmt.Errorf("name is too long (max %d characters)", maxTaskName)
	case utf8.RuneCountInString(desc) > maxTaskDesc:
		return fmt.Errorf("description is too long (max %d characters)", maxTaskDesc)
	}
	return nil
}

func (t *Turn) addTaskTool() llm.Tool {
	return llm.Tool{
		Name: "add_task",
		Description: "Add a task to the requester's own backlog (todo list): something they want to do, track or not forget. " +
			"This is the only place tasks, todos, chores and backlog items go: never save them as facts or memories. " +
			"Tasks are the requester's own; you can't add one for someone else. Only say it is added if this returned added.",
		Schema: map[string]any{"type": "object", "required": []string{"name"}, "properties": map[string]any{
			"name":        map[string]any{"type": "string", "description": "Short task name, under 200 characters."},
			"description": map[string]any{"type": "string", "description": "Optional details."},
			"due":         map[string]any{"type": "string", "description": "Optional local due time, \"YYYY-MM-DD HH:MM\", like <now>."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct{ Name, Description, Due string }
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			in.Name, in.Description = strings.TrimSpace(in.Name), strings.TrimSpace(in.Description)
			if in.Name == "" {
				return "error: name is empty"
			}
			if err := checkTaskText(in.Name, in.Description); err != nil {
				return errText(err)
			}
			td := store.Todo{Owner: t.reqKey(), Name: in.Name, Description: in.Description}
			if in.Due != "" {
				var err error
				if td.Due, err = t.parseDue(in.Due); err != nil {
					return errText(err)
				}
			}
			id, err := t.DB.AddTodo(ctx, td)
			if err != nil {
				return "error: storage failed"
			}
			td.ID, td.Status = id, store.TodoOpen
			log.Printf("add_task %d by %s", id, t.reqKey())
			return "added: " + t.taskLine(td)
		},
	}
}

func (t *Turn) updateTaskTool() llm.Tool {
	return llm.Tool{
		Name: "update_task",
		Description: "Change one of the requester's own tasks by its id from list_tasks: mark it done (or open again), rename it, change its description or due time. " +
			"Only their own tasks: nobody can change another person's. Only say it changed if this returned updated.",
		Schema: map[string]any{"type": "object", "required": []string{"id"}, "properties": map[string]any{
			"id":          map[string]any{"type": "integer", "description": "The task id (\"task 12 | ...\")."},
			"status":      map[string]any{"type": "string", "enum": []string{store.TodoOpen, store.TodoDone}},
			"name":        map[string]any{"type": "string"},
			"description": map[string]any{"type": "string", "description": "Replaces the whole description."},
			"due":         map[string]any{"type": "string", "description": "Local \"YYYY-MM-DD HH:MM\", or \"none\" to remove the due time."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct {
				ID                             int64
				Status, Name, Description, Due *string
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			var c store.TodoChange
			if in.Status != nil {
				s := strings.ToLower(strings.TrimSpace(*in.Status))
				if s != store.TodoOpen && s != store.TodoDone {
					return "error: status must be open or done"
				}
				c.Status = &s
			}
			if in.Name != nil {
				n := strings.TrimSpace(*in.Name)
				if n == "" {
					return "error: name is empty"
				}
				c.Name = &n
			}
			if in.Description != nil {
				d := strings.TrimSpace(*in.Description)
				c.Description = &d
			}
			if err := checkTaskText(deref(c.Name), deref(c.Description)); err != nil {
				return errText(err)
			}
			if in.Due != nil {
				if d := strings.TrimSpace(*in.Due); d == "" || strings.EqualFold(d, "none") {
					c.ClearDue = true
				} else {
					w, err := t.parseDue(d)
					if err != nil {
						return errText(err)
					}
					c.Due = &w
				}
			}
			td, err := t.DB.UpdateTodo(ctx, t.reqKey(), in.ID, c)
			if errors.Is(err, store.ErrNotYourTodo) {
				return "error: the requester has no task with that id; list_tasks shows their tasks"
			}
			if err != nil {
				return "error: storage failed"
			}
			log.Printf("update_task %d by %s", in.ID, t.reqKey())
			return "updated: " + t.taskLine(td)
		},
	}
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func (t *Turn) listTasksTool() llm.Tool {
	return llm.Tool{
		Name: "list_tasks",
		Description: "List or search the requester's own tasks (their backlog, todo list). Always call this before answering anything about their tasks; never answer from memory or the transcript. " +
			"It shows only the requester's tasks: you can't see anyone else's, so if someone asks about another person's tasks, say only that person can ask.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"status": map[string]any{"type": "string", "enum": []string{store.TodoOpen, store.TodoDone, "all"}, "description": "Default open."},
			"query":  map[string]any{"type": "string", "description": "Optional words to find in the name or description."},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			var in struct{ Status, Query string }
			if len(input) > 0 {
				if err := json.Unmarshal(input, &in); err != nil {
					return "error: invalid input: " + err.Error()
				}
			}
			status := strings.ToLower(strings.TrimSpace(in.Status))
			switch status {
			case "":
				status = store.TodoOpen
			case "all":
				status = ""
			case store.TodoOpen, store.TodoDone:
			default:
				return "error: status must be open, done or all"
			}
			got, err := t.DB.ListTodos(ctx, t.reqKey(), status, in.Query, maxTaskList+1)
			if err != nil {
				return "error: storage failed"
			}
			if len(got) == 0 {
				return "no tasks"
			}
			var b strings.Builder
			for i, td := range got {
				if i == maxTaskList {
					b.WriteString("(more: narrow it with query)\n")
					break
				}
				b.WriteString(t.taskLine(td) + "\n")
			}
			return b.String()
		},
	}
}

// taskLine renders a task for the model: "task 12 | open | due Tue
// 2026-11-12 18:00 (in 1d2h) | Name | description".
func (t *Turn) taskLine(td store.Todo) string {
	now := t.now()
	due := "no due time"
	if !td.Due.IsZero() {
		d := td.Due.Sub(now)
		when := "in " + shortDur(d)
		if d < 0 {
			when = shortDur(-d) + " overdue"
			if td.Status == store.TodoDone {
				when = "past"
			}
		}
		due = fmt.Sprintf("due %s (%s)", td.Due.In(now.Location()).Format("Mon 2006-01-02 15:04"), when)
	}
	status := td.Status
	if td.Status == store.TodoDone && !td.Done.IsZero() {
		status = "done " + td.Done.In(now.Location()).Format("2006-01-02")
	}
	line := fmt.Sprintf("task %d | %s | %s | %s", td.ID, status, due, oneLineText(td.Name))
	if d := oneLineText(td.Description); d != "" {
		if utf8.RuneCountInString(d) > 300 {
			d = string([]rune(d)[:300]) + "…"
		}
		line += " | " + d
	}
	return line
}

func oneLineText(s string) string { return strings.Join(strings.Fields(s), " ") }

func shortDur(d time.Duration) string {
	d = d.Round(time.Minute)
	days := int(d / (24 * time.Hour))
	h := int(d % (24 * time.Hour) / time.Hour)
	m := int(d % time.Hour / time.Minute)
	switch {
	case days > 0:
		return fmt.Sprintf("%dd%dh", days, h)
	case h > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	}
	return fmt.Sprintf("%dm", m)
}
