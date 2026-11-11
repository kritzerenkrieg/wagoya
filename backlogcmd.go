package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/config"
	"github.com/kritzerenkrieg/wagoya/internal/store"
	"github.com/kritzerenkrieg/wagoya/internal/tools"
)

// backlogCommand is `wagoya backlog import-vikunja --owner <number>` (U1): a
// one-time copy of a Vikunja instance's tasks into the backlog, owned by
// one person. It uses the host's stored key (I4); running it twice skips
// tasks already imported.
func backlogCommand(args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "import-vikunja" {
		return errors.New("usage: wagoya backlog import-vikunja --owner <number> [--host flow.relow.net] [--dry-run]")
	}
	fs := flag.NewFlagSet("import-vikunja", flag.ContinueOnError)
	owner := fs.String("owner", "", "phone number (digits) of the person who owns the tasks")
	host := fs.String("host", "flow.relow.net", "Vikunja host; its key must be in api_keys.json")
	dry := fs.Bool("dry-run", false, "show what would be imported")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	pn := strings.TrimLeft(strings.TrimSpace(*owner), "+")
	if pn == "" {
		return errors.New("--owner is required")
	}
	if exe, err := os.Executable(); err == nil {
		os.Chdir(filepath.Dir(exe)) // .env and data/ are next to the binary
	}
	if err := config.LoadDotEnv(".env"); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	dir := os.Getenv("DATA_DIR")
	if dir == "" {
		dir = "data"
	}
	keys, err := tools.LoadKeys(filepath.Join(dir, "api_keys.json"))
	if err != nil {
		return err
	}
	headers := keys[*host]
	if headers == nil {
		return fmt.Errorf("no stored key for %s", *host)
	}
	db, err := store.Open(filepath.Join(dir, "bot.db"), 1)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	key, err := db.PersonByPN(ctx, pn)
	if err != nil {
		return err
	}
	vt, projects, err := fetchVikunja(ctx, "https://"+*host+"/api/v1", headers)
	if err != nil {
		return err
	}
	added, skipped := 0, 0
	for _, t := range vt {
		td := vikunjaTodo(t, projects, key)
		if ok, err := db.HasTodoSource(ctx, td.Source); err != nil {
			return err
		} else if ok {
			skipped++
			continue
		}
		if *dry {
			fmt.Fprintf(stdout, "would add %s [%s] %s\n", td.Source, td.Status, td.Name)
			added++
			continue
		}
		if _, err := db.AddTodo(ctx, td); err != nil {
			return fmt.Errorf("%s: %w", td.Source, err)
		}
		added++
	}
	verb := "imported"
	if *dry {
		verb = "would import"
	}
	fmt.Fprintf(stdout, "%s %d tasks for %s (%d already imported)\n", verb, added, store.IDTag(strings.TrimPrefix(key, "pn:")), skipped)
	if *dry {
		return nil
	}
	// The owner's facts about the old board would send Rin back to it.
	n, err := db.RetireFactsMentioning(ctx, key, []string{"vikunja", *host}, "tasks moved to the built-in backlog (U1)")
	if err != nil {
		return err
	}
	if n > 0 {
		if err := db.AddPersonFacts(ctx, key, store.Source{Tier: store.TierOwner}, []string{"Tasks: kept in Rin's own backlog (list_tasks), no longer Vikunja"}); err != nil {
			return err
		}
	}
	fmt.Fprintf(stdout, "retired %d facts about %s\n", n, *host)
	return nil
}

type vikunjaTask struct {
	ID          int64     `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Done        bool      `json:"done"`
	DoneAt      time.Time `json:"done_at"`
	DueDate     time.Time `json:"due_date"`
	Created     time.Time `json:"created"`
	Priority    int       `json:"priority"`
	ProjectID   int64     `json:"project_id"`
	Labels      []struct {
		Title string `json:"title"`
	} `json:"labels"`
}

func fetchVikunja(ctx context.Context, base string, headers map[string]string) ([]vikunjaTask, map[int64]string, error) {
	get := func(path string, out any) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
		if err != nil {
			return err
		}
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("GET %s: %s", path, resp.Status)
		}
		return json.NewDecoder(resp.Body).Decode(out)
	}
	var projs []struct {
		ID    int64  `json:"id"`
		Title string `json:"title"`
	}
	if err := get("/projects?per_page=200", &projs); err != nil {
		return nil, nil, err
	}
	projects := map[int64]string{}
	for _, p := range projs {
		projects[p.ID] = p.Title
	}
	var all []vikunjaTask
	for page := 1; ; page++ {
		var got []vikunjaTask
		if err := get(fmt.Sprintf("/tasks?per_page=200&page=%d", page), &got); err != nil {
			return nil, nil, err
		}
		all = append(all, got...)
		if len(got) < 200 || page == 50 {
			break
		}
	}
	return all, projects, nil
}

var htmlTag = regexp.MustCompile(`(?s)<[^>]*>`)

// vikunjaTodo maps a Vikunja task: project, priority and labels have no
// column, so they go at the end of the description (U1).
func vikunjaTodo(t vikunjaTask, projects map[int64]string, owner string) store.Todo {
	desc := strings.ReplaceAll(t.Description, "</p>", "\n")
	desc = strings.ReplaceAll(desc, "<br>", "\n")
	desc = strings.TrimSpace(html.UnescapeString(htmlTag.ReplaceAllString(desc, "")))
	var extra []string
	if p := projects[t.ProjectID]; p != "" {
		extra = append(extra, "Project: "+p)
	}
	if t.Priority > 0 {
		extra = append(extra, fmt.Sprintf("Priority: %d", t.Priority))
	}
	if len(t.Labels) > 0 {
		var ls []string
		for _, l := range t.Labels {
			ls = append(ls, l.Title)
		}
		extra = append(extra, "Labels: "+strings.Join(ls, ", "))
	}
	if len(extra) > 0 {
		desc = strings.TrimSpace(desc + "\n" + strings.Join(extra, " | "))
	}
	td := store.Todo{Owner: owner, Name: strings.TrimSpace(t.Title), Description: desc, Status: store.TodoOpen,
		Source: fmt.Sprintf("vikunja:%d", t.ID), Created: t.Created}
	if t.DueDate.Year() > 1 {
		td.Due = t.DueDate
	}
	if t.Done {
		td.Status = store.TodoDone
		if t.DoneAt.Year() > 1 {
			td.Done = t.DoneAt
		}
	}
	if td.Created.Year() <= 1 {
		td.Created = time.Time{}
	}
	return td
}
