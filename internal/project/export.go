package project

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/store"
)

// ParseTask parses a task reference like "12" or "#12".
func ParseTask(s string) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(strings.TrimSpace(s), "#"), 10, 64)
	if err != nil || id <= 0 {
		return 0, plyerr.New(plyerr.Invalid, "ply tasks", "%q isn't a task number", s)
	}
	return id, nil
}

// ExportSnapshot picks what to export: the project as of an applied task
// (at), an open task's latest version (task), or the current project.
// It returns the snapshot and a description for the export info file.
func ExportSnapshot(tx *store.Tx, at, task string) (store.Hash, string, error) {
	switch {
	case at != "" && task != "":
		return store.Hash{}, "", plyerr.New(plyerr.Invalid, "", "export takes --at or --task, not both")
	case at != "":
		id, err := ParseTask(at)
		if err != nil {
			return store.Hash{}, "", err
		}
		v, err := tx.VersionOfTask(id)
		if err != nil {
			return store.Hash{}, "", plyerr.New(plyerr.NotFound, "ply log", "task #%d hasn't been applied; use --task to export an open task", id)
		}
		t, _ := tx.Task(id)
		return v.Snapshot, fmt.Sprintf("project #%d, after task #%d %q", v.Num, id, t.Title), nil
	case task != "":
		id, err := ParseTask(task)
		if err != nil {
			return store.Hash{}, "", err
		}
		t, err := tx.Task(id)
		if err != nil {
			return store.Hash{}, "", notFound(err)
		}
		latest, err := tx.TaskSnapshot(t)
		changes, _ := tx.Changes(id)
		return latest, fmt.Sprintf("task #%d %q (%s), %d changes, share %d, built on project #%d",
			id, t.Title, t.Status, len(changes), t.ShareSeq, t.BaseVersion), err
	}
	head, err := tx.Head()
	if err != nil {
		return store.Hash{}, "", err
	}
	desc := fmt.Sprintf("project #%d", head.Num)
	if head.Task != 0 {
		t, _ := tx.Task(head.Task)
		desc += fmt.Sprintf(", last applied task #%d %q", head.Task, t.Title)
	}
	return head.Snapshot, desc, nil
}

// ExportInfo is the text of an export's .plyexport file. It's for people;
// Ply never reads it.
func ExportInfo(projectName, desc string, when time.Time) string {
	return fmt.Sprintf("This is an export of the Ply project %q. Ply doesn't track it; edits here do nothing.\n\nProject:  %s\nContents: %s\nExported: %s\n",
		projectName, projectName, desc, when.UTC().Format(time.RFC3339))
}
