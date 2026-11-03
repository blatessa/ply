// Package client talks to a Ply server: the JSON API, and moving content
// between the server and this machine's project database.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/wire"
)

// Client is a connection to one project on a server.
type Client struct {
	URL   string // project URL, e.g. http://localhost:7070/myproject
	User  string
	Token string
	HTTP  *http.Client
}

func New(projectURL, user, token string) *Client {
	return &Client{URL: strings.TrimRight(projectURL, "/"), User: user, Token: token,
		HTTP: &http.Client{Timeout: 10 * time.Minute}}
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, contentType string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.URL+path, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	req.Header.Set("X-Ply-User", c.User)
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		var ne net.Error
		if errors.As(err, &ne) || strings.Contains(err.Error(), "connection refused") {
			return nil, plyerr.New(plyerr.Internal, "", "can't reach the server at %s (%v)", c.URL, unwrapURLError(err))
		}
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		var e plyerr.Error
		if json.Unmarshal(b, &e) == nil && e.Msg != "" {
			return nil, &e
		}
		return nil, fmt.Errorf("server: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	return resp, nil
}

func unwrapURLError(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

func (c *Client) call(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	ct := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ct = bytes.NewReader(b), "application/json"
	}
	resp, err := c.do(ctx, method, "/api"+path, body, ct)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// State returns the project head and tasks.
func (c *Client) State(ctx context.Context, all bool) (*project.State, error) {
	var s project.State
	q := ""
	if all {
		q = "?all=1"
	}
	return &s, c.call(ctx, "GET", "/state"+q, nil, &s)
}

// Log returns the project's history, newest first.
func (c *Client) Log(ctx context.Context) ([]store.Version, error) {
	var r struct {
		Versions []store.Version `json:"versions"`
	}
	return r.Versions, c.call(ctx, "GET", "/log", nil, &r)
}

// Version returns project version n.
func (c *Client) Version(ctx context.Context, n int64) (store.Version, error) {
	var v store.Version
	return v, c.call(ctx, "GET", fmt.Sprintf("/versions/%d", n), nil, &v)
}

// Task returns a task with its changes and notes.
func (c *Client) Task(ctx context.Context, id int64) (*project.TaskInfo, error) {
	var t project.TaskInfo
	return &t, c.call(ctx, "GET", fmt.Sprintf("/tasks/%d", id), nil, &t)
}

// Upstream returns what a task should be refreshed onto.
func (c *Client) Upstream(ctx context.Context, id int64) (project.Upstream, error) {
	var u project.Upstream
	return u, c.call(ctx, "GET", fmt.Sprintf("/tasks/%d/upstream", id), nil, &u)
}

// CreateTask makes a task built on the project (on == 0) or another task.
func (c *Client) CreateTask(ctx context.Context, title, description string, on int64) (*project.TaskInfo, error) {
	var t project.TaskInfo
	in := map[string]any{"title": title, "description": description, "on": on}
	return &t, c.call(ctx, "POST", "/tasks", in, &t)
}

// Approve applies a task (and tasks under it).
func (c *Client) Approve(ctx context.Context, id int64) ([]store.Version, error) {
	var r struct {
		Versions []store.Version `json:"versions"`
	}
	return r.Versions, c.call(ctx, "POST", fmt.Sprintf("/tasks/%d/approve", id), struct{}{}, &r)
}

// SetStatus marks a task ready, closes it, or reopens it.
func (c *Client) SetStatus(ctx context.Context, id int64, action string) (*project.TaskInfo, error) {
	var t project.TaskInfo
	return &t, c.call(ctx, "POST", fmt.Sprintf("/tasks/%d/%s", id, action), struct{}{}, &t)
}

// AddNote adds a note to a task.
func (c *Client) AddNote(ctx context.Context, id int64, body string) (*store.Note, error) {
	var n store.Note
	return &n, c.call(ctx, "POST", fmt.Sprintf("/tasks/%d/notes", id), map[string]string{"body": body}, &n)
}

// Revert creates a task undoing an applied task.
func (c *Client) Revert(ctx context.Context, id int64) (*project.TaskInfo, error) {
	var t project.TaskInfo
	return &t, c.call(ctx, "POST", fmt.Sprintf("/tasks/%d/revert", id), struct{}{}, &t)
}

// CreateProject makes a new project on the server at serverURL.
func CreateProject(ctx context.Context, serverURL, user, name string) error {
	c := New(strings.TrimRight(serverURL, "/"), user, "")
	return c.call(ctx, "POST", "/projects", map[string]string{"name": name}, nil)
}

// Fetch downloads whatever is needed to have the want snapshots locally,
// skipping content already on this machine. have names snapshots this
// machine already has, to keep the listing short.
func (c *Client) Fetch(ctx context.Context, db *store.DB, want, have []store.Hash) error {
	// Skip snapshots we already have.
	var need []store.Hash
	err := db.Read(ctx, func(tx *store.Tx) error {
		for _, h := range want {
			if ok, err := tx.HasBlob(h); err != nil {
				return err
			} else if !ok {
				need = append(need, h)
			}
		}
		var known []store.Hash
		for _, h := range have {
			if ok, _ := tx.HasBlob(h); ok {
				known = append(known, h)
			}
		}
		have = known
		return nil
	})
	if err != nil || len(need) == 0 {
		return err
	}
	var fr wire.FetchResponse
	if err := c.call(ctx, "POST", "/fetch", wire.FetchRequest{Want: need, Have: have}, &fr); err != nil {
		return err
	}
	var chunks []store.Hash
	var blobs []store.BlobInfo
	err = db.Read(ctx, func(tx *store.Tx) error {
		seen := map[store.Hash]bool{}
		for _, b := range fr.Blobs {
			if ok, err := tx.HasBlob(b.Hash); err != nil {
				return err
			} else if ok {
				continue
			}
			blobs = append(blobs, b)
			for _, ch := range b.Chunks {
				if seen[ch] {
					continue
				}
				seen[ch] = true
				if ok, err := tx.HasChunk(ch); err != nil {
					return err
				} else if !ok {
					chunks = append(chunks, ch)
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	return c.getChunks(ctx, db, chunks, blobs)
}

// getChunks downloads chunks in batches, then records the blobs.
func (c *Client) getChunks(ctx context.Context, db *store.DB, chunks []store.Hash, blobs []store.BlobInfo) error {
	const batch = 2000
	for len(chunks) > 0 {
		n := min(batch, len(chunks))
		part := chunks[:n]
		chunks = chunks[n:]
		b, _ := json.Marshal(wire.ChunksRequest{Chunks: part})
		resp, err := c.do(ctx, "POST", "/api/chunks", bytes.NewReader(b), "application/json")
		if err != nil {
			return err
		}
		pr := wire.NewPackReader(resp.Body)
		want := map[store.Hash]bool{}
		for _, h := range part {
			want[h] = true
		}
		err = db.Write(ctx, func(tx *store.Tx) error {
			for {
				h, data, err := pr.Chunk()
				if errors.Is(err, wire.ErrEnd) {
					break
				}
				if err != nil {
					return fmt.Errorf("download: %w", err)
				}
				if !want[h] {
					return fmt.Errorf("download: server sent unexpected content %s", h.Short())
				}
				if _, err := tx.PutChunk(data); err != nil {
					return err
				}
				delete(want, h)
			}
			if len(want) > 0 {
				return fmt.Errorf("download: server didn't send %d pieces of content", len(want))
			}
			return nil
		})
		resp.Body.Close()
		if err != nil {
			return err
		}
	}
	return db.Write(ctx, func(tx *store.Tx) error { return project.StoreBlobs(tx, blobs) })
}

// Share uploads a task's labeled changes and the content the server lacks.
func (c *Client) Share(ctx context.Context, db *store.DB, req project.ShareRequest) (*project.TaskInfo, error) {
	// Describe every blob the changes use that isn't in the base snapshot,
	// which the server already has.
	var blobs []store.BlobInfo
	err := db.Read(ctx, func(tx *store.Tx) error {
		known := map[store.Hash]bool{}
		if base, err := tx.Snapshot(req.BaseSnapshot); err == nil {
			for _, e := range base.Entries {
				known[e.Hash] = true
			}
		}
		add := func(h store.Hash) error {
			if known[h] {
				return nil
			}
			known[h] = true
			info, err := tx.Blob(h)
			if err != nil {
				return err
			}
			blobs = append(blobs, info)
			return nil
		}
		for _, ch := range append([]store.Change{{Snapshot: req.BaseSnapshot}}, req.Changes...) {
			s, err := tx.Snapshot(ch.Snapshot)
			if err != nil {
				return err
			}
			if err := add(ch.Snapshot); err != nil {
				return err
			}
			for _, e := range s.Entries {
				if err := add(e.Hash); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	var missing wire.MissingResponse
	if err := c.call(ctx, "POST", "/missing", wire.MissingRequest{Blobs: blobs}, &missing); err != nil {
		return nil, err
	}

	// Stream the pack: metadata, then chunks.
	pr, pw := io.Pipe()
	go func() {
		w := wire.NewPackWriter(pw)
		err := w.JSON(wire.ShareMeta{ShareRequest: req, Blobs: blobs})
		if err == nil {
			err = db.Read(ctx, func(tx *store.Tx) error {
				for _, h := range missing.Chunks {
					data, err := tx.Chunk(h)
					if err != nil {
						return err
					}
					if err := w.Chunk(data); err != nil {
						return err
					}
				}
				return nil
			})
		}
		if err == nil {
			err = w.Close()
		}
		pw.CloseWithError(err)
	}()
	resp, err := c.do(ctx, "POST", fmt.Sprintf("/api/tasks/%d/share", req.Task), pr, "application/x-ply-pack")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var info project.TaskInfo
	return &info, json.NewDecoder(resp.Body).Decode(&info)
}

// Export downloads the project (or a task) as a zip.
func (c *Client) Export(ctx context.Context, at, task string, w io.Writer) error {
	q := "?"
	if at != "" {
		q += "at=" + at
	}
	if task != "" {
		q += "task=" + task
	}
	resp, err := c.do(ctx, "GET", "/export"+q, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, err = io.Copy(w, resp.Body)
	return err
}
