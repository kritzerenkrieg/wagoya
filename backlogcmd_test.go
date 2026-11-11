package main

import (
	"testing"
	"time"

	"github.com/kritzerenkrieg/wagoya/internal/store"
)

// U1: a Vikunja task keeps its name, done state and dates; project,
// priority and labels go into the description; HTML is stripped.
func TestVikunjaTodo(t *testing.T) {
	var vt vikunjaTask
	vt.ID, vt.Title, vt.Description, vt.Priority, vt.ProjectID = 7, " Ship demo ", "<p>Build &amp; upload</p><p>itch.io</p>", 3, 3
	vt.Labels = append(vt.Labels, struct {
		Title string `json:"title"`
	}{"jam"})
	vt.Done, vt.DoneAt = true, time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	vt.Created = time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	vt.DueDate = time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC) // Vikunja's "no due date"
	td := vikunjaTodo(vt, map[int64]string{3: "Game Dev"}, "lid1")
	want := "Build & upload\nitch.io\nProject: Game Dev | Priority: 3 | Labels: jam"
	if td.Name != "Ship demo" || td.Description != want || td.Status != store.TodoDone || !td.Done.Equal(vt.DoneAt) ||
		!td.Due.IsZero() || td.Source != "vikunja:7" || td.Owner != "lid1" || !td.Created.Equal(vt.Created) {
		t.Fatalf("%+v\ndesc %q", td, td.Description)
	}
}
