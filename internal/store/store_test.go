package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTest(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "p.ply"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func randBytes(seed uint64, n int) []byte {
	r := rand.New(rand.NewPCG(seed, 0))
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	return b
}

var ctx = context.Background()

func chunkBytes(t *testing.T, db *DB) int64 {
	var n int64
	db.Read(ctx, func(tx *Tx) error {
		return tx.row("SELECT coalesce(sum(length(data)),0) FROM chunk").Scan(&n)
	})
	return n
}

func TestPragmas(t *testing.T) {
	db := openTest(t)
	var mode string
	var page int
	db.w.QueryRow("PRAGMA journal_mode").Scan(&mode)
	db.w.QueryRow("PRAGMA page_size").Scan(&page)
	if mode != "wal" || page != 16384 {
		t.Fatalf("journal_mode=%s page_size=%d", mode, page)
	}
	// Reopening an existing database is fine.
	db.Close()
	db2, err := Open(db.Path)
	if err != nil {
		t.Fatal(err)
	}
	db2.Close()
}

func TestBlobRoundTripAndDedup(t *testing.T) {
	db := openTest(t)
	for _, n := range []int{0, 1, 100, 200_000, 5 << 20} {
		data := randBytes(uint64(n), n)
		var h Hash
		err := db.Write(ctx, func(tx *Tx) (err error) {
			h, err = tx.PutBlobBytes(data)
			return
		})
		if err != nil {
			t.Fatal(err)
		}
		before := chunkBytes(t, db)
		db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes(data); return err })
		if chunkBytes(t, db) != before {
			t.Fatalf("n=%d: storing same content twice grew the store", n)
		}
		var got []byte
		db.Read(ctx, func(tx *Tx) (err error) { got, err = tx.ReadBlob(h); return })
		if !bytes.Equal(got, data) {
			t.Fatalf("n=%d: content differs", n)
		}
	}
}

func TestSmallEditStoresLittle(t *testing.T) {
	db := openTest(t)
	data := randBytes(9, 20<<20)
	db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes(data); return err })
	before := chunkBytes(t, db)
	edited := bytes.Clone(data)
	copy(edited[10<<20:], "a small edit in the middle")
	db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes(edited); return err })
	if grew := chunkBytes(t, db) - before; grew > 300<<10 {
		t.Fatalf("a small edit to a 20 MiB file stored %d bytes", grew)
	}
}

func TestCompression(t *testing.T) {
	db := openTest(t)
	text := bytes.Repeat([]byte("func main() { fmt.Println(\"hello\") }\n"), 5000)
	db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes(text); return err })
	if n := chunkBytes(t, db); n > int64(len(text))/5 {
		t.Fatalf("text stored as %d bytes, want compressed", n)
	}
}

func TestWriteIsAtomic(t *testing.T) {
	db := openTest(t)
	boom := errors.New("boom")
	before := chunkBytes(t, db)
	err := db.Write(ctx, func(tx *Tx) error {
		tx.PutBlobBytes([]byte("never committed"))
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if n := chunkBytes(t, db); n != before {
		t.Fatalf("failed write left %d bytes", n-before)
	}
}

func TestSnapshotRoundTripAndDiff(t *testing.T) {
	db := openTest(t)
	var a, b *Snapshot
	var ha, hb Hash
	err := db.Write(ctx, func(tx *Tx) error {
		f1, _ := tx.PutBlobBytes([]byte("one"))
		f2, _ := tx.PutBlobBytes([]byte("two"))
		f3, _ := tx.PutBlobBytes([]byte("three"))
		a = &Snapshot{Entries: []Entry{
			{Path: "src/main.go", Mode: File, Hash: f1, Size: 3},
			{Path: "README", Mode: File, Hash: f2, Size: 3},
			{Path: "run.sh", Mode: Exec, Hash: f3, Size: 5},
		}}
		b = &Snapshot{Entries: []Entry{
			{Path: "README", Mode: File, Hash: f3, Size: 5},
			{Path: "run.sh", Mode: Exec, Hash: f3, Size: 5},
			{Path: "new file with spaces.txt", Mode: File, Hash: f1, Size: 3},
		}}
		var err error
		if ha, err = tx.PutSnapshot(a); err != nil {
			return err
		}
		hb, err = tx.PutSnapshot(b)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	var a2 *Snapshot
	db.Read(ctx, func(tx *Tx) (err error) { a2, err = tx.Snapshot(ha); return })
	if len(a2.Entries) != 3 || a2.Entries[0].Path != "README" {
		t.Fatalf("round trip: %+v", a2.Entries)
	}
	if e, ok := a2.Get("run.sh"); !ok || e.Mode != Exec {
		t.Fatalf("Get run.sh: %+v %v", e, ok)
	}
	var b2 *Snapshot
	db.Read(ctx, func(tx *Tx) (err error) { b2, err = tx.Snapshot(hb); return })
	d := Diff(a2, b2)
	want := []struct {
		p string
		k DeltaKind
	}{{"README", Modified}, {"new file with spaces.txt", Added}, {"src/main.go", Removed}}
	if len(d) != len(want) {
		t.Fatalf("diff: %+v", d)
	}
	for i, w := range want {
		if d[i].Path != w.p || d[i].Kind != w.k {
			t.Fatalf("diff[%d] = %s %s, want %s %s", i, d[i].Kind, d[i].Path, w.k, w.p)
		}
	}
}

func TestConcurrentReadersWithWriter(t *testing.T) {
	db := openTest(t)
	var h Hash
	db.Write(ctx, func(tx *Tx) (err error) { h, err = tx.PutBlobBytes(randBytes(1, 1<<20)); return })
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			errs <- db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes(randBytes(uint64(i+100), 300_000)); return err })
		}()
		go func() {
			defer wg.Done()
			errs <- db.Read(ctx, func(tx *Tx) error { _, err := tx.ReadBlob(h); return err })
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPathWithSpaces(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "Application Support", "ply #1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, "my project.ply")
	db, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes([]byte("x")); return err }); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("database not at expected path: %v", err)
	}
}

func BenchmarkPutBlob(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "p.ply"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	b.SetBytes(64 << 20)
	for i := range b.N {
		data := randBytes(uint64(i), 64<<20)
		db.Write(ctx, func(tx *Tx) error { _, err := tx.PutBlobBytes(data); return err })
	}
}

func BenchmarkReadBlob(b *testing.B) {
	db, err := Open(filepath.Join(b.TempDir(), "p.ply"))
	if err != nil {
		b.Fatal(err)
	}
	defer db.Close()
	var h Hash
	db.Write(ctx, func(tx *Tx) (err error) { h, err = tx.PutBlobBytes(randBytes(1, 64<<20)); return })
	b.SetBytes(64 << 20)
	for b.Loop() {
		db.Read(ctx, func(tx *Tx) error { return tx.WriteBlob(io.Discard, h) })
	}
}
