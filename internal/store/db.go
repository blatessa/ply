// Package store is Ply's SQLite storage: one database file per project holding
// file contents (as deduplicated chunks), snapshots, and records.
//
// The same schema is used on the server and on each machine. Locally the
// database also holds workspaces, saves and the undo log, which never leave
// the machine.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"

	_ "modernc.org/sqlite"
)

// DB is an open project database. Reads run concurrently; writes are
// serialized through a single connection and each runs in one transaction.
type DB struct {
	r    *sql.DB
	w    *sql.DB
	Path string
}

func dsn(path string, readonly bool) string {
	q := url.Values{}
	for _, p := range []string{
		"journal_mode(WAL)",
		"synchronous(NORMAL)",
		"busy_timeout(10000)",
		"foreign_keys(ON)",
		"cache_size(-65536)",    // 64 MiB
		"mmap_size(1073741824)", // 1 GiB
		"temp_store(MEMORY)",
	} {
		q.Add("_pragma", p)
	}
	if readonly {
		q.Add("mode", "ro")
	} else {
		q.Set("_txlock", "immediate")
	}
	return "file:" + url.PathEscape(path) + "?" + q.Encode()
}

// Open opens (creating if needed) the database at path and brings its schema
// up to date.
func Open(path string) (*DB, error) {
	if err := create(path); err != nil {
		return nil, fmt.Errorf("create %s: %w", path, err)
	}
	w, err := sql.Open("sqlite", dsn(path, false))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	db := &DB{w: w, Path: path}
	if err := db.migrate(); err != nil {
		w.Close()
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	// The empty snapshot is the project before its first task.
	err = db.Write(context.Background(), func(t *Tx) error {
		_, err := t.PutSnapshot(&Snapshot{})
		return err
	})
	if err != nil {
		w.Close()
		return nil, err
	}
	r, err := sql.Open("sqlite", dsn(path, true))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(max(4, runtime.NumCPU()))
	db.r = r
	return db, nil
}

// create makes a new database file with a larger page size. The page size
// can only be set before the file has content or is in WAL mode.
func create(path string) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	d, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		return err
	}
	defer d.Close()
	_, err = d.Exec("PRAGMA page_size = 16384; VACUUM;")
	return err
}

func (db *DB) Close() error {
	return errors.Join(db.r.Close(), db.w.Close())
}

// Tx is a transaction. All store operations go through one.
type Tx struct {
	tx *sql.Tx
	// Writable reports whether this transaction may write.
	Writable bool
}

// Write runs fn in a write transaction, committing if fn returns nil.
// Writes are all-or-nothing.
func (db *DB) Write(ctx context.Context, fn func(*Tx) error) error {
	return run(ctx, db.w, true, fn)
}

// Read runs fn in a read transaction with a consistent view of the database.
func (db *DB) Read(ctx context.Context, fn func(*Tx) error) error {
	return run(ctx, db.r, false, fn)
}

func run(ctx context.Context, d *sql.DB, writable bool, fn func(*Tx) error) (err error) {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: !writable})
	if err != nil {
		return err
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
		if err != nil {
			tx.Rollback()
			return
		}
		err = tx.Commit()
	}()
	return fn(&Tx{tx: tx, Writable: writable})
}

func (t *Tx) exec(q string, args ...any) (sql.Result, error) { return t.tx.Exec(q, args...) }
func (t *Tx) query(q string, args ...any) (*sql.Rows, error) { return t.tx.Query(q, args...) }
func (t *Tx) row(q string, args ...any) *sql.Row             { return t.tx.QueryRow(q, args...) }

func (db *DB) migrate() error {
	var v int
	if err := db.w.QueryRow("PRAGMA user_version").Scan(&v); err != nil {
		return err
	}
	if v > len(migrations) {
		return fmt.Errorf("database is from a newer version of ply (schema %d); run `ply upgrade`", v)
	}
	for i := v; i < len(migrations); i++ {
		err := run(context.Background(), db.w, true, func(t *Tx) error {
			if _, err := t.exec(migrations[i]); err != nil {
				return err
			}
			_, err := t.exec(fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
	}
	return nil
}

// Exec, Query and QueryRow give packages that own local-only tables (such as
// the workspace scan cache) direct SQL access within the transaction.
func (t *Tx) Exec(q string, args ...any) (sql.Result, error) { return t.tx.Exec(q, args...) }
func (t *Tx) Query(q string, args ...any) (*sql.Rows, error) { return t.tx.Query(q, args...) }
func (t *Tx) QueryRow(q string, args ...any) *sql.Row        { return t.tx.QueryRow(q, args...) }
