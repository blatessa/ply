package local

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/blatessa/ply/internal/store"
)

// After a switch, an editor that still has files open from before can save
// them, quietly putting the previous task's version into this one. Ply
// spots writes whose content is exactly what the folder showed before the
// switch, in files the switch changed, and reports them until the file
// changes again.

// StaleWrite is a file that was probably saved from an editor buffer opened
// before the last switch.
type StaleWrite struct {
	Path string     `json:"path"`
	From int64      `json:"from_task"` // what the folder showed before (0: the project)
	Hash store.Hash `json:"content"`
	// Save is the save just before the write, for `ply undo <save>`.
	Save int64 `json:"save_before"`
}

// StaleWrites returns the folder's likely stale writes.
func (w *WS) StaleWrites(ctx context.Context) ([]StaleWrite, error) {
	var out []StaleWrite
	err := w.DB.Read(ctx, func(tx *store.Tx) error {
		var err error
		out, err = w.staleWrites(tx)
		return err
	})
	return out, err
}

// noteSwitch remembers what the folder showed before and after a switch.
func (w *WS) noteSwitch(tx *store.Tx, prevTask int64, prev, next store.Hash) error {
	_, err := tx.Exec("UPDATE workspace SET prev_task = ?, prev_files = ?, next_files = ?, stale = '[]' WHERE id = ?",
		prevTask, prev[:], next[:], w.ID)
	return err
}

func (w *WS) staleWrites(tx *store.Tx) ([]StaleWrite, error) {
	var s string
	if err := tx.QueryRow("SELECT stale FROM workspace WHERE id = ?", w.ID).Scan(&s); err != nil {
		return nil, err
	}
	var out []StaleWrite
	return out, json.Unmarshal([]byte(s), &out)
}

// checkStale updates the folder's stale writes for the files on disk now,
// before they're recorded as a save.
func (w *WS) checkStale(tx *store.Tx, snap *store.Snapshot) error {
	var prevTask int64
	var prevB, nextB []byte
	var js string
	err := tx.QueryRow("SELECT prev_task, prev_files, next_files, stale FROM workspace WHERE id = ?", w.ID).Scan(&prevTask, &prevB, &nextB, &js)
	if err != nil || len(prevB) != 32 || len(nextB) != 32 {
		return err
	}
	var stale []StaleWrite
	if err := json.Unmarshal([]byte(js), &stale); err != nil {
		return err
	}
	changed := false
	// Drop writes that have since been replaced.
	keep := stale[:0]
	for _, s := range stale {
		if e, ok := snap.Get(s.Path); ok && e.Hash == s.Hash {
			keep = append(keep, s)
		} else {
			changed = true
		}
	}
	stale = keep
	// New writes since the last save.
	task, folder := w.timeline()
	var lastID int64
	var lastB []byte
	err = tx.QueryRow("SELECT id, snapshot FROM save WHERE task = ? AND workspace = ? ORDER BY id DESC LIMIT 1", task, folder).Scan(&lastID, &lastB)
	if errors.Is(err, sql.ErrNoRows) {
		err = nil
	} else if err == nil {
		last, err := tx.Snapshot(store.Hash(lastB))
		if err != nil {
			return err
		}
		ds := store.Diff(last, snap)
		if len(ds) > 0 {
			prev, err := tx.Snapshot(store.Hash(prevB))
			if err != nil {
				return err
			}
			next, err := tx.Snapshot(store.Hash(nextB))
			if err != nil {
				return err
			}
			for _, d := range ds {
				if d.Kind == store.Removed {
					continue
				}
				pe, ok := prev.Get(d.Path)
				if !ok || pe.Hash != d.New.Hash {
					continue
				}
				if ne, ok := next.Get(d.Path); ok && ne.Hash == pe.Hash {
					continue // the switch didn't change this file
				}
				stale = append(stale, StaleWrite{Path: d.Path, From: prevTask, Hash: d.New.Hash, Save: lastID})
				changed = true
			}
		}
	}
	if err != nil || !changed {
		return err
	}
	if stale == nil {
		stale = []StaleWrite{}
	}
	b, _ := json.Marshal(stale)
	_, err = tx.Exec("UPDATE workspace SET stale = ? WHERE id = ?", string(b), w.ID)
	return err
}
