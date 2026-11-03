package workspace

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/blatessa/ply/internal/store"
)

var ctx = context.Background()

func setup(t *testing.T) (*store.DB, string) {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "p.ply"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	root := t.TempDir()
	db.Write(ctx, func(tx *store.Tx) error {
		_, err := tx.Exec("INSERT INTO workspace (id, path, task, created_at) VALUES (1, ?, 1, 0)", root)
		return err
	})
	return db, root
}

func write(t *testing.T, root, p, content string) {
	t.Helper()
	full := filepath.Join(root, p)
	os.MkdirAll(filepath.Dir(full), 0o755)
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func scan(t *testing.T, db *store.DB, root string) *store.Snapshot {
	t.Helper()
	var s *store.Snapshot
	err := db.Write(ctx, func(tx *store.Tx) (err error) { s, err = Scan(tx, 1, root); return })
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func paths(s *store.Snapshot) []string {
	var out []string
	for _, e := range s.Entries {
		out = append(out, e.Path)
	}
	return out
}

func TestScan(t *testing.T) {
	db, root := setup(t)
	write(t, root, "main.go", "package main\n")
	write(t, root, "src/lib/a.txt", "a")
	write(t, root, "node_modules/x/index.js", "ignored")
	write(t, root, ".ply", "{}")
	write(t, root, "debug.log", "ignored by .plyignore")
	write(t, root, ".plyignore", "*.log\n")
	os.Symlink("main.go", filepath.Join(root, "link"))
	os.WriteFile(filepath.Join(root, "run.sh"), []byte("#!/bin/sh\n"), 0o755)

	s := scan(t, db, root)
	got := paths(s)
	want := []string{".plyignore", "link", "main.go", "run.sh", "src/lib/a.txt"}
	if len(got) != len(want) {
		t.Fatalf("scanned %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("scanned %v, want %v", got, want)
		}
	}
	if e, _ := s.Get("run.sh"); e.Mode != store.Exec {
		t.Errorf("run.sh mode %c", e.Mode)
	}
	if e, _ := s.Get("link"); e.Mode != store.Symlink {
		t.Errorf("link mode %c", e.Mode)
	}

	// Editing a file is picked up even within the racy window.
	write(t, root, "main.go", "package main // edited\n")
	s2 := scan(t, db, root)
	a, _ := s.Get("main.go")
	b, _ := s2.Get("main.go")
	if a.Hash == b.Hash {
		t.Fatal("edit not detected")
	}
	os.Remove(filepath.Join(root, "src/lib/a.txt"))
	if _, ok := scan(t, db, root).Get("src/lib/a.txt"); ok {
		t.Fatal("removal not detected")
	}
}

func TestMaterializeAndApply(t *testing.T) {
	db, root := setup(t)
	write(t, root, "a.txt", "one")
	write(t, root, "dir/b.txt", "two")
	write(t, root, "node_modules/keep.js", "ignored, must survive")
	os.WriteFile(filepath.Join(root, "x.sh"), []byte("echo\n"), 0o755)
	s1 := scan(t, db, root)

	// Change to a second state.
	write(t, root, "a.txt", "one, changed")
	os.RemoveAll(filepath.Join(root, "dir"))
	write(t, root, "dir", "now a file")
	s2 := scan(t, db, root)

	// Apply back to s1 in place.
	err := db.Write(ctx, func(tx *store.Tx) error { return Apply(tx, 1, root, s2, s1, nil) })
	if err != nil {
		t.Fatal(err)
	}
	if got := scan(t, db, root); len(store.Diff(got, s1)) != 0 {
		t.Fatalf("after apply: %+v", store.Diff(got, s1))
	}
	if _, err := os.Stat(filepath.Join(root, "node_modules/keep.js")); err != nil {
		t.Fatal("ignored file removed")
	}

	// Materialize into a fresh folder, cloning from the first workspace.
	root2 := t.TempDir()
	err = db.Write(ctx, func(tx *store.Tx) error {
		tx.Exec("INSERT INTO workspace (id, path, task, created_at) VALUES (2, ?, 2, 0)", root2)
		donors, err := LoadDonors(tx)
		if err != nil {
			return err
		}
		return Materialize(tx, 2, root2, s1, donors)
	})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(root2, "dir/b.txt"))
	if string(b) != "two" {
		t.Fatalf("dir/b.txt = %q", b)
	}
	if info, _ := os.Stat(filepath.Join(root2, "x.sh")); info.Mode().Perm()&0o100 == 0 {
		t.Fatal("exec bit lost")
	}
	if err := CopyIgnored(root, root2); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(root2, "node_modules/keep.js")); string(b) != "ignored, must survive" {
		t.Fatal("copy-ignored didn't copy node_modules")
	}
}
