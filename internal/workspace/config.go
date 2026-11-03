// Package workspace manages this machine's side of Ply: the joined projects,
// workspace folders, recording saves, and writing snapshots out to disk.
package workspace

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
)

// Home returns the directory holding this machine's Ply data: config and
// one database per joined project. PLY_HOME overrides it.
func Home() string {
	if h := os.Getenv("PLY_HOME"); h != "" {
		return h
	}
	home, _ := os.UserHomeDir()
	if runtime.GOOS == "darwin" {
		return filepath.Join(home, "Library", "Application Support", "ply")
	}
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, "ply")
	}
	return filepath.Join(home, ".local", "share", "ply")
}

// Project is a project this machine has joined.
type Project struct {
	URL   string `json:"url"`  // e.g. http://localhost:7070/myproject
	Name  string `json:"name"` // the last path segment of URL
	DB    string `json:"db"`   // local database path
	Dir   string `json:"dir"`  // where new workspaces are created
	Token string `json:"token,omitempty"`
}

// Config is this machine's Ply configuration.
type Config struct {
	User     string     `json:"user"`
	Projects []*Project `json:"projects"`
}

func configPath() string { return filepath.Join(Home(), "config.json") }

// LoadConfig reads the config, returning defaults if there is none.
func LoadConfig() (*Config, error) {
	c := &Config{}
	b, err := os.ReadFile(configPath())
	if err == nil {
		err = json.Unmarshal(b, c)
	} else if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	if c.User == "" {
		c.User = defaultUser()
	}
	return c, err
}

func defaultUser() string {
	if u := os.Getenv("PLY_USER"); u != "" {
		return u
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "me"
}

// Save writes the config atomically.
func (c *Config) Save() error {
	if err := os.MkdirAll(Home(), 0o755); err != nil {
		return err
	}
	b, _ := json.MarshalIndent(c, "", "  ")
	return writeFileAtomic(configPath(), b, 0o600)
}

// Find returns the joined project with the given URL or name.
func (c *Config) Find(urlOrName string) *Project {
	for _, p := range c.Projects {
		if p.URL == urlOrName || p.Name == urlOrName {
			return p
		}
	}
	return nil
}

// ProjectForDir returns the project whose workspace folder contains dir, if
// any.
func (c *Config) ProjectForDir(dir string) *Project {
	for _, p := range c.Projects {
		if rel, err := filepath.Rel(p.Dir, dir); err == nil && !strings.HasPrefix(rel, "..") {
			return p
		}
	}
	return nil
}

// AddProject registers a joined project and returns it. dir is where its
// workspaces will be created.
func (c *Config) AddProject(rawURL, dir string) (*Project, error) {
	u, err := url.Parse(strings.TrimRight(rawURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("%q isn't a project address; it should look like https://code.example.com/myproject", rawURL)
	}
	name := filepath.Base(u.Path)
	if name == "/" || name == "." || name == "" {
		return nil, fmt.Errorf("%q has no project name; it should look like %s://%s/myproject", rawURL, u.Scheme, u.Host)
	}
	clean := u.String()
	if p := c.Find(clean); p != nil {
		p.Dir = dir
		return p, nil
	}
	if p := c.Find(name); p != nil {
		return nil, fmt.Errorf("a project named %q is already joined from %s", name, p.URL)
	}
	host := strings.NewReplacer(":", "_", "/", "_").Replace(u.Host)
	p := &Project{
		URL:  clean,
		Name: name,
		DB:   filepath.Join(Home(), "projects", host, name+".ply"),
		Dir:  dir,
	}
	c.Projects = append(c.Projects, p)
	return p, os.MkdirAll(filepath.Dir(p.DB), 0o755)
}

// Server is the project's server address, and Name its name there.
func (p *Project) Server() string {
	return strings.TrimSuffix(p.URL, "/"+p.Name)
}

func writeFileAtomic(path string, b []byte, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
