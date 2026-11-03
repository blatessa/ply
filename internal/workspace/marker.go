package workspace

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// MarkerName is the file at a workspace's root that ties it to a task.
const MarkerName = ".ply"

// Marker is the content of a workspace's .ply file.
type Marker struct {
	Project string `json:"project"`          // project URL
	Task    int64  `json:"task"`             // the task shown; 0 is the project itself
	Title   string `json:"title,omitempty"`  // the task's title, for status bars and prompts
	Folder  int64  `json:"folder,omitempty"` // this folder's ID on this machine
}

// Workspace is a workspace folder on this machine.
type Workspace struct {
	Root string
	Marker
}

// ErrNotInWorkspace means the current directory isn't inside a workspace.
var ErrNotInWorkspace = errors.New("not in a workspace")

// FindWorkspace returns the workspace containing dir.
func FindWorkspace(dir string) (*Workspace, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, MarkerName))
		if err == nil {
			w := &Workspace{Root: dir}
			if err := json.Unmarshal(b, &w.Marker); err != nil {
				return nil, err
			}
			return w, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil, ErrNotInWorkspace
		}
		dir = parent
	}
}

// WriteMarker writes the workspace's .ply file.
func (w *Workspace) WriteMarker() error {
	b, _ := json.MarshalIndent(w.Marker, "", "  ")
	return writeFileAtomic(filepath.Join(w.Root, MarkerName), append(b, '\n'), 0o644)
}
