package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/blatessa/ply/internal/merge"
	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/workspace"
)

// wsState is what an action changes: the task's local base and conflicts,
// its change list, and (for actions that rewrite files) the files.
type wsState struct {
	BaseTask     int64            `json:"base_task"`
	BaseVersion  int64            `json:"base_version"`
	BaseSnapshot store.Hash       `json:"base_snapshot"`
	Conflicts    []store.Conflict `json:"conflicts"`
	Changes      []store.Change   `json:"changes"`
	Files        *store.Hash      `json:"files,omitempty"`
}

type actionData struct {
	Desc   string  `json:"desc"`
	Before wsState `json:"before"`
	After  wsState `json:"after"`
	Undoes int64   `json:"undoes,omitempty"`
}

func captureState(tx *store.Tx, t store.Task) (wsState, error) {
	cs, err := tx.Changes(t.ID)
	return wsState{BaseTask: t.BaseTask, BaseVersion: t.BaseVersion, BaseSnapshot: t.BaseSnapshot,
		Conflicts: t.Conflicts, Changes: cs}, err
}

func withFiles(s wsState, h store.Hash) wsState {
	s.Files = &h
	return s
}

func recordAction(tx *store.Tx, wsID int64, kind, desc string, before, after wsState) error {
	b, err := json.Marshal(actionData{Desc: desc, Before: before, After: after})
	if err != nil {
		return err
	}
	_, err = tx.Exec("INSERT INTO action (workspace, kind, data, at) VALUES (?, ?, ?, ?)", wsID, kind, string(b), time.Now().UnixMilli())
	return err
}

// restore puts the workspace back into state s. cur is the files on disk
// now. If s has files and from is the file state the action being reversed
// produced, edits made since then are carried over by a three-way merge.
func (w *WS) restore(tx *store.Tx, s wsState, cur *store.Snapshot, curHash store.Hash, from *store.Hash) ([]string, error) {
	t, err := tx.Task(w.Task)
	if err != nil {
		return nil, err
	}
	t.BaseTask, t.BaseVersion, t.BaseSnapshot, t.Conflicts = s.BaseTask, s.BaseVersion, s.BaseSnapshot, s.Conflicts
	var notices []string
	if s.Files != nil {
		target, err := tx.Snapshot(*s.Files)
		if err != nil {
			return nil, err
		}
		if from != nil && *from != curHash {
			base, err := tx.Snapshot(*from)
			if err != nil {
				return nil, err
			}
			res, err := merge.Trees(tx, base, cur, target, merge.Labels{Ours: "your later edits", Theirs: "undone"})
			if err != nil {
				return nil, err
			}
			target = res.Snapshot
			t.Conflicts = append(t.Conflicts, res.Conflicts...)
			notices = append(notices, res.Notices...)
			if len(res.Conflicts) > 0 {
				notices = append(notices, fmt.Sprintf("%d files conflict with edits made since; see `ply conflicts`", len(res.Conflicts)))
			}
		}
		if err := workspace.Apply(tx, w.ID, w.Root, cur, target, nil); err != nil {
			return nil, err
		}
	}
	if err := tx.PutTask(&t); err != nil {
		return nil, err
	}
	if _, err := tx.Exec("DELETE FROM change WHERE task = ?", w.Task); err != nil {
		return nil, err
	}
	for _, c := range s.Changes {
		if err := tx.PutChange(&c); err != nil {
			return nil, err
		}
	}
	return notices, nil
}

// Action is an entry in a workspace's action log.
type Action struct {
	ID     int64     `json:"id"`
	Kind   string    `json:"kind"`
	Desc   string    `json:"desc"`
	At     time.Time `json:"at"`
	Undone bool      `json:"undone"`
}

// UndoResult says what undo or redo did.
type UndoResult struct {
	Did     string   `json:"did"`
	Notices []string `json:"notices,omitempty"`
}

func loadAction(tx *store.Tx, q string, args ...any) (int64, actionData, error) {
	var id int64
	var data string
	var a actionData
	err := tx.QueryRow(q, args...).Scan(&id, &data)
	if err != nil {
		return 0, a, err
	}
	return id, a, json.Unmarshal([]byte(data), &a)
}

// Undo reverses the last action in the workspace that hasn't been undone;
// running it again goes further back. With a save ID, it returns the files to
// that save instead (which is itself an action that can be undone).
func (w *WS) Undo(ctx context.Context, saveID int64) (*UndoResult, error) {
	res := &UndoResult{}
	err := w.DB.Write(ctx, func(tx *store.Tx) error {
		curSnap, cur, err := w.catchUp(tx)
		if err != nil {
			return err
		}
		t, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		now, err := captureState(tx, t)
		if err != nil {
			return err
		}

		if saveID != 0 {
			var snap []byte
			var at int64
			err := tx.QueryRow("SELECT snapshot, at FROM save WHERE id = ? AND workspace = ?", saveID, w.ID).Scan(&snap, &at)
			if errors.Is(err, sql.ErrNoRows) {
				return plyerr.New(plyerr.NotFound, "ply history", "no save %d in this workspace", saveID)
			}
			if err != nil {
				return err
			}
			target := withFiles(now, store.Hash(snap))
			if _, err := w.restore(tx, target, curSnap, cur, nil); err != nil {
				return err
			}
			res.Did = fmt.Sprintf("went back to save %d (%s)", saveID, time.UnixMilli(at).Format("Jan 2 15:04:05"))
			if _, _, err := w.catchUp(tx); err != nil {
				return err
			}
			return recordAction(tx, w.ID, "goto", res.Did, withFiles(now, cur), target)
		}

		id, a, err := loadAction(tx, "SELECT id, data FROM action WHERE workspace = ? AND undone = 0 AND kind != 'undo' ORDER BY id DESC LIMIT 1", w.ID)
		if errors.Is(err, sql.ErrNoRows) {
			return plyerr.New(plyerr.State, "", "nothing to undo")
		}
		if err != nil {
			return err
		}
		before := now
		if a.Before.Files != nil {
			before = withFiles(now, cur)
		}
		if res.Notices, err = w.restore(tx, a.Before, curSnap, cur, a.After.Files); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE action SET undone = 1 WHERE id = ?", id); err != nil {
			return err
		}
		if _, _, err := w.catchUp(tx); err != nil {
			return err
		}
		res.Did = "undid " + a.Desc
		after, err := tx.Task(w.Task)
		if err != nil {
			return err
		}
		afterState, err := captureState(tx, after)
		if err != nil {
			return err
		}
		if a.Before.Files != nil {
			afterState.Files = a.Before.Files
		}
		b, _ := json.Marshal(actionData{Desc: a.Desc, Before: before, After: afterState, Undoes: id})
		_, err = tx.Exec("INSERT INTO action (workspace, kind, data, at) VALUES (?, 'undo', ?, ?)", w.ID, string(b), time.Now().UnixMilli())
		return err
	})
	return res, err
}

// Redo reverses the most recent undo, as long as nothing else has been done
// since.
func (w *WS) Redo(ctx context.Context) (*UndoResult, error) {
	res := &UndoResult{}
	err := w.DB.Write(ctx, func(tx *store.Tx) error {
		curSnap, cur, err := w.catchUp(tx)
		if err != nil {
			return err
		}
		var lastAction int64
		tx.QueryRow("SELECT coalesce(max(id), 0) FROM action WHERE workspace = ? AND kind != 'undo'", w.ID).Scan(&lastAction)
		id, a, err := loadAction(tx, "SELECT id, data FROM action WHERE workspace = ? AND kind = 'undo' AND undone = 0 AND id > ? ORDER BY id DESC LIMIT 1", w.ID, lastAction)
		if errors.Is(err, sql.ErrNoRows) {
			return plyerr.New(plyerr.State, "", "nothing to redo")
		}
		if err != nil {
			return err
		}
		if res.Notices, err = w.restore(tx, a.Before, curSnap, cur, a.After.Files); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE action SET undone = 1 WHERE id = ?", id); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE action SET undone = 0 WHERE id = ?", a.Undoes); err != nil {
			return err
		}
		res.Did = "redid " + a.Desc
		_, _, err = w.catchUp(tx)
		return err
	})
	return res, err
}

// Event is an entry in a workspace's timeline.
type Event struct {
	Kind   string     `json:"kind"` // "save", "change", or an action kind
	ID     any        `json:"id"`
	At     time.Time  `json:"at"`
	Desc   string     `json:"desc"`
	Files  int        `json:"files_changed,omitempty"`
	Hash   store.Hash `json:"snapshot,omitempty"`
	Undone bool       `json:"undone,omitempty"`
}

// History returns the workspace's timeline, newest first: saves, labeled
// changes and actions.
func (w *WS) History(ctx context.Context, limit int) ([]Event, error) {
	if _, _, err := w.CatchUp(ctx); err != nil {
		return nil, err
	}
	var out []Event
	err := w.DB.Read(ctx, func(tx *store.Tx) error {
		rows, err := tx.Query("SELECT id, snapshot, at FROM save WHERE workspace = ? ORDER BY id DESC LIMIT ?", w.ID, limit+1)
		if err != nil {
			return err
		}
		type save struct {
			id   int64
			snap store.Hash
			at   int64
		}
		var saves []save
		for rows.Next() {
			var s save
			var b []byte
			if err := rows.Scan(&s.id, &b, &s.at); err != nil {
				rows.Close()
				return err
			}
			s.snap = store.Hash(b)
			saves = append(saves, s)
		}
		rows.Close()
		for i, s := range saves {
			if i == limit {
				break
			}
			e := Event{Kind: "save", ID: s.id, At: time.UnixMilli(s.at), Hash: s.snap}
			if i+1 < len(saves) {
				a, err1 := tx.Snapshot(saves[i+1].snap)
				b, err2 := tx.Snapshot(s.snap)
				if err1 == nil && err2 == nil {
					d := store.Diff(a, b)
					e.Files = len(d)
					e.Desc = summarize(d)
				}
			} else {
				e.Desc = "workspace created"
			}
			out = append(out, e)
		}
		changes, err := tx.Changes(w.Task)
		if err != nil {
			return err
		}
		for _, c := range changes {
			desc := c.Label
			if !c.Shared {
				desc += " (not shared)"
			}
			out = append(out, Event{Kind: "change", ID: c.ID, At: c.UpdatedAt, Desc: desc, Hash: c.Snapshot})
		}
		arows, err := tx.Query("SELECT id, kind, data, at, undone FROM action WHERE workspace = ? AND kind NOT IN ('label', 'label-edit') ORDER BY id DESC LIMIT ?", w.ID, limit)
		if err != nil {
			return err
		}
		defer arows.Close()
		for arows.Next() {
			var id, at int64
			var kind, data string
			var undone bool
			if err := arows.Scan(&id, &kind, &data, &at, &undone); err != nil {
				return err
			}
			var a actionData
			json.Unmarshal([]byte(data), &a)
			out = append(out, Event{Kind: kind, ID: id, At: time.UnixMilli(at), Desc: a.Desc, Undone: undone})
		}
		return nil
	})
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, err
}

func summarize(d []store.Delta) string {
	switch len(d) {
	case 0:
		return "no file changes"
	case 1:
		return fmt.Sprintf("%s %s", d[0].Kind, d[0].Path)
	}
	return fmt.Sprintf("%s %s and %d more", d[0].Kind, d[0].Path, len(d)-1)
}
