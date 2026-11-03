// Package server is Ply in server mode: an HTTP API over any number of
// projects, each in its own database file.
package server

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/blatessa/ply/internal/plyerr"
	"github.com/blatessa/ply/internal/project"
	"github.com/blatessa/ply/internal/store"
	"github.com/blatessa/ply/internal/wire"
)

// Server serves projects stored under Dir.
type Server struct {
	Dir string
	Log *log.Logger

	mu       sync.Mutex
	projects map[string]*project.Project
}

func New(dir string) (*Server, error) {
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		return nil, err
	}
	return &Server{Dir: dir, projects: map[string]*project.Project{}, Log: log.New(os.Stderr, "", log.LstdFlags)}, nil
}

// Close closes all open project databases.
func (s *Server) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var errs []error
	for _, p := range s.projects {
		errs = append(errs, p.DB.Close())
	}
	s.projects = map[string]*project.Project{}
	return errors.Join(errs...)
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

func (s *Server) dbPath(name string) string {
	return filepath.Join(s.Dir, "projects", name+".ply")
}

// Project opens a project by name.
func (s *Server) Project(name string) (*project.Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p, ok := s.projects[name]; ok {
		return p, nil
	}
	if !validName.MatchString(name) {
		return nil, plyerr.New(plyerr.NotFound, "", "no project named %q", name)
	}
	path := s.dbPath(name)
	if _, err := os.Stat(path); err != nil {
		return nil, plyerr.New(plyerr.NotFound, "ply server new "+name, "no project named %q on this server", name)
	}
	db, err := store.Open(path)
	if err != nil {
		return nil, err
	}
	p := project.New(name, db)
	s.projects[name] = p
	return p, nil
}

// CreateProject makes a new, empty project.
func (s *Server) CreateProject(name string) error {
	if !validName.MatchString(name) {
		return plyerr.New(plyerr.Invalid, "", "%q isn't a valid project name: use lowercase letters, digits, '.', '-' and '_'", name)
	}
	s.mu.Lock()
	path := s.dbPath(name)
	if _, err := os.Stat(path); err == nil {
		s.mu.Unlock()
		return plyerr.New(plyerr.Invalid, "", "a project named %q already exists", name)
	}
	db, err := store.Open(path)
	if err == nil {
		s.projects[name] = project.New(name, db)
	}
	s.mu.Unlock()
	return err
}

// Projects lists project names.
func (s *Server) Projects() ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(s.Dir, "projects"))
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if n, ok := strings.CutSuffix(e.Name(), ".ply"); ok {
			out = append(out, n)
		}
	}
	return out, nil
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/projects", s.wrap(func(r *http.Request) (any, error) {
		names, err := s.Projects()
		return map[string]any{"projects": names}, err
	}))
	mux.HandleFunc("POST /api/projects", s.wrap(func(r *http.Request) (any, error) {
		var req struct {
			Name string `json:"name"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return map[string]string{"name": req.Name}, s.CreateProject(req.Name)
	}))

	p := func(pattern string, h func(*project.Project, *http.Request) (any, error)) {
		method, path, _ := strings.Cut(pattern, " ")
		mux.HandleFunc(method+" /{project}/api"+path, s.wrap(func(r *http.Request) (any, error) {
			proj, err := s.Project(r.PathValue("project"))
			if err != nil {
				return nil, err
			}
			return h(proj, r)
		}))
	}
	p("GET /state", func(pr *project.Project, r *http.Request) (any, error) {
		return pr.State(r.Context(), r.URL.Query().Get("all") == "1")
	})
	p("GET /log", func(pr *project.Project, r *http.Request) (any, error) {
		vs, err := pr.Log(r.Context())
		return map[string]any{"versions": vs}, err
	})
	p("GET /versions/{num}", func(pr *project.Project, r *http.Request) (any, error) {
		n, err := strconv.ParseInt(r.PathValue("num"), 10, 64)
		if err != nil {
			return nil, plyerr.New(plyerr.Invalid, "", "bad version number")
		}
		var v store.Version
		err = pr.DB.Read(r.Context(), func(tx *store.Tx) (err error) { v, err = tx.Version(n); return })
		if errors.Is(err, store.ErrNotFound) {
			return nil, plyerr.New(plyerr.NotFound, "ply log", "project #%d doesn't exist", n)
		}
		return v, err
	})
	p("POST /tasks", func(pr *project.Project, r *http.Request) (any, error) {
		var req struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			On          int64  `json:"on"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return pr.CreateTask(r.Context(), actor(r), req.Title, req.Description, req.On)
	})
	p("GET /tasks/{id}", func(pr *project.Project, r *http.Request) (any, error) {
		id, err := taskID(r)
		if err != nil {
			return nil, err
		}
		return pr.Task(r.Context(), id)
	})
	p("GET /tasks/{id}/upstream", func(pr *project.Project, r *http.Request) (any, error) {
		id, err := taskID(r)
		if err != nil {
			return nil, err
		}
		return pr.Upstream(r.Context(), id)
	})
	p("POST /tasks/{id}/approve", func(pr *project.Project, r *http.Request) (any, error) {
		id, err := taskID(r)
		if err != nil {
			return nil, err
		}
		vs, err := pr.Approve(r.Context(), actor(r), id)
		return map[string]any{"versions": vs}, err
	})
	for _, st := range []struct{ path, status string }{
		{"ready", store.StatusReady}, {"close", store.StatusClosed}, {"reopen", store.StatusOpen},
	} {
		p("POST /tasks/{id}/"+st.path, func(pr *project.Project, r *http.Request) (any, error) {
			id, err := taskID(r)
			if err != nil {
				return nil, err
			}
			return pr.SetStatus(r.Context(), actor(r), id, st.status)
		})
	}
	p("POST /tasks/{id}/notes", func(pr *project.Project, r *http.Request) (any, error) {
		id, err := taskID(r)
		if err != nil {
			return nil, err
		}
		var req struct {
			Body string `json:"body"`
		}
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		return pr.AddNote(r.Context(), actor(r), id, req.Body)
	})
	p("POST /tasks/{id}/revert", func(pr *project.Project, r *http.Request) (any, error) {
		id, err := taskID(r)
		if err != nil {
			return nil, err
		}
		return pr.Revert(r.Context(), actor(r), id)
	})
	p("POST /missing", func(pr *project.Project, r *http.Request) (any, error) {
		var req wire.MissingRequest
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		var resp wire.MissingResponse
		err := pr.DB.Read(r.Context(), func(tx *store.Tx) (err error) {
			resp.Chunks, err = project.MissingChunks(tx, req.Blobs)
			return
		})
		return resp, err
	})
	p("POST /fetch", func(pr *project.Project, r *http.Request) (any, error) {
		var req wire.FetchRequest
		if err := decode(r, &req); err != nil {
			return nil, err
		}
		var resp wire.FetchResponse
		err := pr.DB.Read(r.Context(), func(tx *store.Tx) (err error) {
			resp.Blobs, err = project.SnapshotBlobs(tx, req.Want, req.Have)
			return
		})
		return resp, err
	})
	p("POST /tasks/{id}/share", s.share)
	mux.HandleFunc("POST /{project}/api/chunks", s.chunks)
	mux.HandleFunc("GET /{project}/export", s.export)
	return mux
}

// actor is who is making the request.
// TODO(M5): replace with authenticated accounts (#30).
func actor(r *http.Request) string {
	if u := r.Header.Get("X-Ply-User"); u != "" {
		return u
	}
	return "anonymous"
}

func taskID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.PathValue("id"), "#"), 10, 64)
	if err != nil {
		return 0, plyerr.New(plyerr.Invalid, "ply tasks", "%q isn't a task number", r.PathValue("id"))
	}
	return id, nil
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(v); err != nil {
		return plyerr.New(plyerr.Invalid, "", "bad request: %v", err)
	}
	return nil
}

func (s *Server) wrap(h func(*http.Request) (any, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, err := h(r)
		if err != nil {
			writeError(w, s.Log, err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(v)
	}
}

func writeError(w http.ResponseWriter, l *log.Logger, err error) {
	e, ok := plyerr.As(err)
	if !ok {
		l.Printf("internal error: %v", err)
		e = &plyerr.Error{Code: plyerr.Internal, Msg: err.Error()}
	}
	status := map[string]int{
		plyerr.NotFound:  http.StatusNotFound,
		plyerr.Invalid:   http.StatusBadRequest,
		plyerr.Stale:     http.StatusConflict,
		plyerr.Conflict:  http.StatusConflict,
		plyerr.State:     http.StatusConflict,
		plyerr.Forbidden: http.StatusForbidden,
	}[e.Code]
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(e)
}

// share receives a pack: metadata, then the chunks the server said it was
// missing. Everything is stored in one transaction.
func (s *Server) share(pr *project.Project, r *http.Request) (any, error) {
	id, err := taskID(r)
	if err != nil {
		return nil, err
	}
	// Read the whole upload before taking the write lock, so a slow client
	// doesn't hold up everyone else.
	tmp, err := os.CreateTemp("", "ply-share-*")
	if err != nil {
		return nil, err
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, err := io.Copy(tmp, r.Body); err != nil {
		return nil, err
	}
	if _, err := tmp.Seek(0, 0); err != nil {
		return nil, err
	}
	pack := wire.NewPackReader(tmp)
	var meta wire.ShareMeta
	if err := pack.JSON(&meta); err != nil {
		return nil, plyerr.New(plyerr.Invalid, "", "bad share upload: %v", err)
	}
	meta.Task = id
	var info *project.TaskInfo
	err = pr.DB.Write(r.Context(), func(tx *store.Tx) error {
		for {
			_, data, err := pack.Chunk()
			if errors.Is(err, wire.ErrEnd) {
				break
			}
			if err != nil {
				return plyerr.New(plyerr.Invalid, "", "bad share upload: %v", err)
			}
			if _, err := tx.PutChunk(data); err != nil {
				return err
			}
		}
		if err := project.StoreBlobs(tx, meta.Blobs); err != nil {
			return err
		}
		var err error
		info, err = pr.Share(tx, actor(r), meta.ShareRequest)
		return err
	})
	return info, err
}

// chunks sends the requested chunks as a pack.
func (s *Server) chunks(w http.ResponseWriter, r *http.Request) {
	pr, err := s.Project(r.PathValue("project"))
	if err != nil {
		writeError(w, s.Log, err)
		return
	}
	var req wire.ChunksRequest
	if err := decode(r, &req); err != nil {
		writeError(w, s.Log, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ply-pack")
	pw := wire.NewPackWriter(w)
	err = pr.DB.Read(r.Context(), func(tx *store.Tx) error {
		for _, h := range req.Chunks {
			data, err := tx.Chunk(h)
			if err != nil {
				return err
			}
			if err := pw.Chunk(data); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		s.Log.Printf("chunks: %v", err)
		return // the client sees a truncated pack
	}
	pw.Close()
}

// export sends the project's files (now, ?at=<task>, or ?task=<task>) as a zip.
func (s *Server) export(w http.ResponseWriter, r *http.Request) {
	pr, err := s.Project(r.PathValue("project"))
	if err != nil {
		writeError(w, s.Log, err)
		return
	}
	var snap *store.Snapshot
	var info string
	err = pr.DB.Read(r.Context(), func(tx *store.Tx) error {
		h, desc, err := project.ExportSnapshot(tx, r.URL.Query().Get("at"), r.URL.Query().Get("task"))
		if err != nil {
			return err
		}
		info = project.ExportInfo(pr.Name, desc, time.Now())
		snap, err = tx.Snapshot(h)
		return err
	})
	if err != nil {
		writeError(w, s.Log, err)
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", pr.Name+".zip"))
	zw := zip.NewWriter(w)
	err = pr.DB.Read(r.Context(), func(tx *store.Tx) error {
		for _, e := range snap.Entries {
			hdr := &zip.FileHeader{Name: e.Path, Method: zip.Deflate}
			switch e.Mode {
			case store.Exec:
				hdr.SetMode(0o755)
			case store.Symlink:
				hdr.SetMode(os.ModeSymlink | 0o777)
			default:
				hdr.SetMode(0o644)
			}
			fw, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			if err := tx.WriteBlob(fw, e.Hash); err != nil {
				return err
			}
		}
		if r.URL.Query().Get("info") != "0" {
			fw, err := zw.Create(".plyexport")
			if err != nil {
				return err
			}
			io.WriteString(fw, info)
		}
		return nil
	})
	if err != nil {
		s.Log.Printf("export: %v", err)
	}
	zw.Close()
}

// ListenAndServe serves until ctx is done.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	srv := &http.Server{Addr: addr, Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(sctx)
	}()
	err := srv.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
