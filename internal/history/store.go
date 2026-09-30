// Package history persists per-scope focus history (current/previous ids plus a
// capped ring) as atomic JSON files under the plugin state directory.
package history

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// ringCap bounds the retained history ring per scope. A current/previous pair is
// sufficient for an A<->B toggle; the ring is kept for future use.
const ringCap = 200

// Record is the persisted per-scope history. Current and Previous drive the
// toggle; Ring keeps recent ids most-recent-first.
type Record struct {
	Current  string   `json:"current"`
	Previous string   `json:"previous"`
	Ring     []string `json:"ring,omitempty"`
}

// Store reads and writes scope history files under a base directory.
type Store struct {
	dir string
}

// New returns a Store rooted at dir, creating the directory if needed.
func New(dir string) (*Store, error) {
	if dir == "" {
		return nil, fmt.Errorf("history: empty state dir")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: dir}, nil
}

// path returns the absolute path for a scope file name.
func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name)
}

// WorkspaceFile is the workspace scope file name.
func WorkspaceFile() string { return "workspace.json" }

// TabFile is the tab scope file name for a workspace.
func TabFile(workspaceID string) string {
	return fmt.Sprintf("tab.%s.json", workspaceID)
}

// PaneFile is the pane scope file name for a tab.
func PaneFile(tabID string) string {
	return fmt.Sprintf("pane.%s.json", tabID)
}

// Load reads a scope record by file name. A missing file yields a zero Record.
func (s *Store) Load(name string) (Record, error) {
	var rec Record
	b, err := os.ReadFile(s.path(name))
	if err != nil {
		if os.IsNotExist(err) {
			return rec, nil
		}
		return rec, err
	}
	if len(b) == 0 {
		return rec, nil
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		return Record{}, fmt.Errorf("history: parse %s: %w", name, err)
	}
	return rec, nil
}

// save writes a record atomically (temp file + rename).
func (s *Store) save(name string, rec Record) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	dst := s.path(name)
	tmp, err := os.CreateTemp(s.dir, name+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, dst)
}

// Seed initialises a scope's current id without recording a transition. It only
// writes when there is no stored current yet, preserving history across restarts.
func (s *Store) Seed(name, id string) error {
	if id == "" {
		return nil
	}
	rec, err := s.Load(name)
	if err != nil {
		return err
	}
	if rec.Current != "" {
		return nil
	}
	rec.Current = id
	rec.Ring = pushRing(rec.Ring, id)
	return s.save(name, rec)
}

// Update records a focus change for a scope: it shifts current->previous and
// sets the new current only when id differs from the stored current. It returns
// whether a change was recorded.
func (s *Store) Update(name, id string) (bool, error) {
	if id == "" {
		return false, nil
	}
	rec, err := s.Load(name)
	if err != nil {
		return false, err
	}
	if rec.Current == id {
		return false, nil
	}
	if rec.Current != "" {
		rec.Previous = rec.Current
	}
	rec.Current = id
	rec.Ring = pushRing(rec.Ring, id)
	if err := s.save(name, rec); err != nil {
		return false, err
	}
	return true, nil
}

// Dir returns the base directory of the store.
func (s *Store) Dir() string { return s.dir }

// pushRing prepends id (most-recent-first), removing an earlier occurrence and
// capping the length at ringCap.
func pushRing(ring []string, id string) []string {
	out := make([]string, 0, len(ring)+1)
	out = append(out, id)
	for _, v := range ring {
		if v == id {
			continue
		}
		out = append(out, v)
	}
	if len(out) > ringCap {
		out = out[:ringCap]
	}
	return out
}
