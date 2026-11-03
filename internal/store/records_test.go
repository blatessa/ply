package store

import (
	"errors"
	"testing"
)

func TestRecords(t *testing.T) {
	db := openTest(t)
	err := db.Write(ctx, func(tx *Tx) error {
		if _, err := tx.Snapshot(EmptySnapshot); err != nil {
			t.Fatalf("empty snapshot not stored: %v", err)
		}
		head, _ := tx.Head()
		if head.Num != 0 || head.Snapshot != EmptySnapshot {
			t.Fatalf("head of new project: %+v", head)
		}
		a := &Task{Title: "Fix login bug", Owner: "brady", BaseSnapshot: EmptySnapshot}
		if err := tx.PutTask(a); err != nil {
			return err
		}
		b := &Task{Title: "Add password reset", Owner: "brady", BaseTask: a.ID, BaseSnapshot: EmptySnapshot}
		if err := tx.PutTask(b); err != nil {
			return err
		}
		if a.ID != 1 || b.ID != 2 {
			t.Fatalf("ids %d %d", a.ID, b.ID)
		}
		c := &Change{ID: "c1", Task: a.ID, Seq: 1, Label: "first", Snapshot: EmptySnapshot, Author: "brady"}
		if err := tx.PutChange(c); err != nil {
			return err
		}
		c.Label = "first, edited"
		if err := tx.PutChange(c); err != nil {
			return err
		}
		// Updating a task with changes must not trip foreign keys.
		a.Status = StatusReady
		if err := tx.PutTask(a); err != nil {
			return err
		}
		got, _ := tx.Task(a.ID)
		if got.Status != StatusReady || got.Title != "Fix login bug" {
			t.Fatalf("task: %+v", got)
		}
		on, _ := tx.TasksOn(a.ID)
		if len(on) != 1 || on[0].ID != b.ID {
			t.Fatalf("TasksOn: %+v", on)
		}
		ch, _ := tx.Changes(a.ID)
		if len(ch) != 1 || ch[0].Label != "first, edited" {
			t.Fatalf("changes: %+v", ch)
		}
		if err := tx.PutVersion(&Version{Num: 1, Task: a.ID, Snapshot: EmptySnapshot, AppliedBy: "brady"}); err != nil {
			return err
		}
		head, _ = tx.Head()
		if head.Num != 1 || head.Task != a.ID {
			t.Fatalf("head: %+v", head)
		}
		n := &Note{Task: a.ID, Author: "sam", Body: "looks good"}
		if err := tx.PutNote(n); err != nil {
			return err
		}
		notes, _ := tx.Notes(a.ID)
		if len(notes) != 1 || notes[0].Body != "looks good" {
			t.Fatalf("notes: %+v", notes)
		}
		if _, err := tx.Task(99); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing task: %v", err)
		}
		open, _ := tx.Tasks(StatusOpen)
		if len(open) != 1 || open[0].ID != b.ID {
			t.Fatalf("open tasks: %+v", open)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
