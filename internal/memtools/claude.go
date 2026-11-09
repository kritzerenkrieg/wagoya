package memtools

// O1-O5: owners hand coding tasks to Claude Code, which runs on the owner's
// laptop and pulls the queue from here. These tools are given only in an
// owner's turn, and each one checks again.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/kritzerenkrieg/wagoya/internal/llm"
	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// Markers the server puts on the first line of Claude Code messages (O5).
const (
	MarkInitiated = "[Orchestration Initiated]"
	MarkCompleted = "[Orchestration Completed]"
	MarkQuestion  = "[Orchestration Question]"
)

const maxTaskText = 4000

// ClaudeTools returns the Claude Code tools for an owner's turn, or nothing.
func (t *Turn) ClaudeTools() []llm.Tool {
	if !t.Requester.Whitelisted || t.Curator {
		return nil
	}
	return t.claudeTools()
}

func (t *Turn) claudeTools() []llm.Tool {
	return []llm.Tool{t.askClaudeTool(), t.answerClaudeTool(), t.claudeTasksTool(), t.cancelClaudeTool(), t.pushClaudeTool()}
}

// Queued reports whether this turn queued work for Claude Code, so the reply
// gets the MarkInitiated line (O5).
func (t *Turn) Queued() bool { return t.queued.Load() > 0 }

// owner returns the requester's key, or an error text if they aren't an owner.
func (t *Turn) owner() (string, string) {
	key := t.reqKey()
	if !t.Requester.Whitelisted || key == "" {
		return "", "error: only an owner can use Claude Code"
	}
	return key, ""
}

// ownTask loads a task of the requester's.
func (t *Turn) ownTask(ctx context.Context, key string, id int64) (store.Task, string) {
	task, ok, err := t.DB.TaskByID(ctx, id)
	switch {
	case err != nil:
		log.Printf("claude task %d: %v", id, err)
		return task, "error: storage failed"
	case !ok || task.Requester != key:
		return task, fmt.Sprintf("error: you have no task #%d", id)
	}
	return task, ""
}

func (t *Turn) runnerLine(ctx context.Context) string {
	seen, _ := t.DB.Runner(ctx)
	switch {
	case seen.IsZero():
		return "The laptop runner has never connected."
	case t.now().Sub(seen) < store.RunnerOnline:
		return "The laptop runner is online."
	}
	return fmt.Sprintf("The laptop runner looks offline (last seen %s ago); tasks wait until it's back.", t.now().Sub(seen).Round(time.Minute))
}

// repoName is a folder name the runner may create for a new project (Q1).
var repoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

func (t *Turn) askClaudeTool() llm.Tool {
	return llm.Tool{
		Name: "ask_claude_code",
		Description: "Owner only. Hand a coding task to Claude Code, which runs on the owner's laptop in the repo's own folder, as if the owner opened it there. " +
			"Anything that needs permission is sent to the owner to answer yes or no; the server handles those answers, not you. Use it when an owner asks you to get Claude (Code) to change, fix, add or look into code. " +
			"task: what to do, in English, complete and specific, keeping every detail the owner gave. repo: the repo's folder name, from the claude_tasks repo list; any listed repo works with nothing to set up first. " +
			"new_repo: true only when the owner asks for a new project; repo is then its new folder name (letters, digits, - and _), which must not exist yet. " +
			"follow_up_of: a task id to continue the same Claude Code session and branch (\"also add a test for #3\"). " +
			"Returns the task number; the server starts your reply with " + MarkInitiated + " and later posts the result itself, so don't promise a time.",
		Schema: map[string]any{"type": "object", "required": []string{"task"}, "properties": map[string]any{
			"task":         map[string]any{"type": "string"},
			"repo":         map[string]any{"type": "string"},
			"follow_up_of": map[string]any{"type": "integer"},
			"new_repo":     map[string]any{"type": "boolean"},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			key, bad := t.owner()
			if bad != "" {
				return bad
			}
			var in struct {
				Task       string
				Repo       string
				FollowUpOf int64 `json:"follow_up_of"`
				NewRepo    bool  `json:"new_repo"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			in.Task, in.Repo = strings.TrimSpace(in.Task), strings.TrimSpace(in.Repo)
			switch {
			case utf8.RuneCountInString(in.Task) < 5:
				return "error: task is empty"
			case utf8.RuneCountInString(in.Task) > maxTaskText:
				return fmt.Sprintf("error: task is too long (max %d characters)", maxTaskText)
			}
			if n, err := t.DB.OpenTasks(ctx, key); err != nil {
				return "error: storage failed"
			} else if n >= store.MaxOpenTasks {
				return fmt.Sprintf("error: you already have %d unfinished tasks; wait for one or cancel one", n)
			}
			task := store.Task{Chat: t.Chat, Requester: key, RequesterName: t.Requester.SenderName,
				OwnerText: t.Requester.Text, Text: in.Task, FollowUpOf: in.FollowUpOf}
			if in.FollowUpOf != 0 {
				p, bad := t.ownTask(ctx, key, in.FollowUpOf)
				if bad != "" {
					return bad
				}
				if !slices.Contains([]string{store.TaskDone, store.TaskFailed, store.TaskPushed, store.TaskPushFailed}, p.Status) {
					return fmt.Sprintf("error: task #%d is %s; follow up once it has finished", p.ID, human(p.Status))
				}
			} else {
				_, repos := t.DB.Runner(ctx)
				if len(repos) == 0 {
					return "error: the laptop runner has never reported any repos; it needs to be started on the laptop first"
				}
				if in.Repo == "" && len(repos) == 1 && !in.NewRepo {
					in.Repo = repos[0]
				}
				switch {
				case in.NewRepo && !repoName.MatchString(in.Repo):
					return "error: a new repo needs a folder name of letters, digits, - and _ (up to 64)"
				case in.NewRepo && slices.Contains(repos, in.Repo):
					return fmt.Sprintf("error: repo %q already exists; leave out new_repo to work in it", in.Repo)
				case !in.NewRepo && !slices.Contains(repos, in.Repo):
					return fmt.Sprintf("error: repo must be one of: %s (or set new_repo for a new project)", strings.Join(repos, ", "))
				}
				task.Repo, task.NewRepo = in.Repo, in.NewRepo
			}
			id, err := t.DB.AddTask(ctx, task)
			if err != nil {
				log.Printf("add claude task: %v", err)
				return "error: storage failed"
			}
			t.queued.Add(1)
			log.Printf("queued claude task #%d for %s in %s", id, key, t.Chat)
			return fmt.Sprintf("queued as #%d. %s", id, t.runnerLine(ctx))
		},
	}
}

func (t *Turn) answerClaudeTool() llm.Tool {
	return llm.Tool{
		Name: "answer_claude_code",
		Description: "Owner only. Use it only when the owner's current message answers the question Claude Code asked on a task waiting on input " +
			"(see claude_tasks or your " + MarkQuestion + " message). Never for anything else: not for a status question (\"orchestration status?\"), " +
			"thanks, small talk or a new request; for status use claude_tasks. The server sends the owner's message exactly as they wrote it, " +
			"with nothing added, and Claude Code carries on in the same session.",
		Schema: map[string]any{"type": "object", "required": []string{"task_id"}, "properties": map[string]any{
			"task_id": map[string]any{"type": "integer"},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			key, bad := t.owner()
			if bad != "" {
				return bad
			}
			var in struct {
				TaskID int64 `json:"task_id"`
			}
			if err := json.Unmarshal(input, &in); err != nil {
				return "error: invalid input: " + err.Error()
			}
			// O8: the owner's own words, unmodified, are the answer.
			if strings.TrimSpace(t.Requester.Text) == "" {
				return "error: the owner's message is empty"
			}
			if _, bad := t.ownTask(ctx, key, in.TaskID); bad != "" {
				return bad
			}
			if err := t.DB.AnswerTask(ctx, in.TaskID, t.Requester.Text); err != nil {
				return errText(err)
			}
			t.queued.Add(1)
			return fmt.Sprintf("answer queued for #%d. %s", in.TaskID, t.runnerLine(ctx))
		},
	}
}

func (t *Turn) claudeTasksTool() llm.Tool {
	return llm.Tool{
		Name: "claude_tasks",
		Description: "Owner only. List the owner's recent Claude Code tasks with their status, whether the laptop runner is online, " +
			"and every repo it can work in. Check it before telling the owner a repo isn't available. " +
			"task_id: one of the owner's tasks, to get its full report, for questions about what Claude Code found or did.",
		Schema: map[string]any{"type": "object", "properties": map[string]any{
			"task_id": map[string]any{"type": "integer"},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			key, bad := t.owner()
			if bad != "" {
				return bad
			}
			var in struct {
				TaskID int64 `json:"task_id"`
			}
			json.Unmarshal(input, &in)
			if in.TaskID != 0 { // R3: one task's full report
				x, bad := t.ownTask(ctx, key, in.TaskID)
				if bad != "" {
					return bad
				}
				var b strings.Builder
				fmt.Fprintf(&b, "#%d [%s] in %s", x.ID, human(x.Status), x.Repo)
				if x.Branch != "" {
					b.WriteString(", branch " + x.Branch)
				}
				fmt.Fprintf(&b, "\ntask: %s", x.Text)
				if x.Question != "" {
					b.WriteString("\nasks: " + x.Question)
				}
				if x.Approval != "" {
					b.WriteString("\nwaiting on the owner's yes or no to: " + x.Approval)
				}
				if r := strings.TrimSpace(x.Result); r != "" {
					b.WriteString("\nfull report:\n" + r)
				}
				return b.String()
			}
			ts, err := t.DB.RecentTasks(ctx, key, 8)
			if err != nil {
				return "error: storage failed"
			}
			var b strings.Builder
			b.WriteString(t.runnerLine(ctx))
			if _, repos := t.DB.Runner(ctx); len(repos) > 0 { // Q1
				b.WriteString("\nRepos on the laptop: " + strings.Join(repos, ", ") + ".")
			}
			if len(ts) == 0 {
				b.WriteString("\nNo tasks yet.")
			}
			for _, x := range ts {
				fmt.Fprintf(&b, "\n#%d [%s] %s (%s", x.ID, human(x.Status), clip(x.Text, 120), x.Repo)
				if x.Branch != "" {
					b.WriteString(", branch " + x.Branch)
				}
				b.WriteString(")")
				if x.Approval != "" {
					b.WriteString("\n  waiting on the owner's yes or no to: " + clip(x.Approval, 300))
				}
				if x.Question != "" {
					b.WriteString("\n  asks: " + clip(x.Question, 300))
				} else if x.Result != "" {
					b.WriteString("\n  result: " + clip(x.Result, 300))
				}
			}
			return b.String()
		},
	}
}

func (t *Turn) cancelClaudeTool() llm.Tool {
	return llm.Tool{
		Name:        "cancel_claude_task",
		Description: "Owner only. Cancel one of the owner's Claude Code tasks that hasn't finished. Commits already made stay on its branch.",
		Schema: map[string]any{"type": "object", "required": []string{"task_id"}, "properties": map[string]any{
			"task_id": map[string]any{"type": "integer"},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			return t.onTask(ctx, input, func(id int64) (string, error) {
				return fmt.Sprintf("cancelled #%d", id), t.DB.CancelTask(ctx, id)
			})
		},
	}
}

func (t *Turn) pushClaudeTool() llm.Tool {
	return llm.Tool{
		Name: "push_claude_branch",
		Description: "Owner only, and only when the owner asks to push. Push an older Claude Code task's claude/task-N branch to GitHub; " +
			"newer tasks have no such branch, so for them hand Claude Code the push as a follow-up instead (it asks the owner to confirm). " +
			"Merging and deploying stay with the owner.",
		Schema: map[string]any{"type": "object", "required": []string{"task_id"}, "properties": map[string]any{
			"task_id": map[string]any{"type": "integer"},
		}},
		Run: func(ctx context.Context, input json.RawMessage) string {
			return t.onTask(ctx, input, func(id int64) (string, error) {
				if err := t.DB.RequestPush(ctx, id); err != nil {
					return "", err
				}
				t.queued.Add(1)
				return fmt.Sprintf("push of #%d queued. %s", id, t.runnerLine(ctx)), nil
			})
		},
	}
}

// onTask runs do on one of the owner's tasks, given {"task_id": N}.
func (t *Turn) onTask(ctx context.Context, input json.RawMessage, do func(id int64) (string, error)) string {
	key, bad := t.owner()
	if bad != "" {
		return bad
	}
	var in struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return "error: invalid input: " + err.Error()
	}
	if _, bad := t.ownTask(ctx, key, in.TaskID); bad != "" {
		return bad
	}
	out, err := do(in.TaskID)
	if err != nil {
		return errText(err)
	}
	return out
}

func human(status string) string { return strings.ReplaceAll(status, "_", " ") }

func clip(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// WithMarker puts mark on the first line of text, once (O5).
func WithMarker(mark, text string) string {
	text = strings.TrimSpace(text)
	if strings.HasPrefix(text, mark) {
		text = strings.TrimSpace(strings.TrimPrefix(text, mark))
	}
	return mark + "\n" + text
}
