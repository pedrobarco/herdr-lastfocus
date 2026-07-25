package history

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestFileNames(t *testing.T) {
	if got := WorkspaceFile(); got != "workspace.json" {
		t.Errorf("WorkspaceFile() = %q", got)
	}
	if got := TabFile("w1"); got != "tab.w1.json" {
		t.Errorf("TabFile(w1) = %q", got)
	}
	if got := PaneFile("w1:t1"); got != "pane.w1:t1.json" {
		t.Errorf("PaneFile(w1:t1) = %q", got)
	}
}

func TestLoadMissingIsZero(t *testing.T) {
	s := newStore(t)
	rec, err := s.Load("nope.json")
	if err != nil {
		t.Fatalf("Load missing: %v", err)
	}
	if rec.Current != "" || rec.Previous != "" || len(rec.Ring) != 0 {
		t.Errorf("Load missing = %+v, want zero", rec)
	}
}

func TestUpdateShiftsCurrentToPrevious(t *testing.T) {
	s := newStore(t)
	name := WorkspaceFile()

	changed, err := s.Update(name, "a")
	if err != nil || !changed {
		t.Fatalf("first Update: changed=%v err=%v", changed, err)
	}
	rec := load(t, s, name)
	if rec.Current != "a" || rec.Previous != "" {
		t.Fatalf("after a: %+v", rec)
	}

	changed, err = s.Update(name, "b")
	if err != nil || !changed {
		t.Fatalf("second Update: changed=%v err=%v", changed, err)
	}
	rec = load(t, s, name)
	if rec.Current != "b" || rec.Previous != "a" {
		t.Fatalf("after b: %+v", rec)
	}
}

func TestUpdateSameIDIsNoop(t *testing.T) {
	s := newStore(t)
	name := WorkspaceFile()

	if _, err := s.Update(name, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(name, "b"); err != nil {
		t.Fatal(err)
	}
	// Repeating the current id must not shift previous.
	changed, err := s.Update(name, "b")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("Update(same current) reported changed")
	}
	rec := load(t, s, name)
	if rec.Current != "b" || rec.Previous != "a" {
		t.Fatalf("after repeat: %+v", rec)
	}
}

func TestUpdateEmptyIDIgnored(t *testing.T) {
	s := newStore(t)
	changed, err := s.Update(WorkspaceFile(), "")
	if err != nil {
		t.Fatal(err)
	}
	if changed {
		t.Errorf("Update(empty) reported changed")
	}
	if _, err := os.Stat(filepath.Join(s.dir, WorkspaceFile())); !os.IsNotExist(err) {
		t.Errorf("empty Update created a file")
	}
}

func TestToggleBackAndForth(t *testing.T) {
	s := newStore(t)
	name := TabFile("w1")

	for _, id := range []string{"t1", "t2", "t1", "t2"} {
		if _, err := s.Update(name, id); err != nil {
			t.Fatal(err)
		}
	}
	rec := load(t, s, name)
	// A<->B toggle: last two distinct ids are current=t2, previous=t1.
	if rec.Current != "t2" || rec.Previous != "t1" {
		t.Fatalf("toggle result: %+v", rec)
	}
}

func TestSeedOnlyWhenUnset(t *testing.T) {
	s := newStore(t)
	name := WorkspaceFile()

	if err := s.Seed(name, "a"); err != nil {
		t.Fatal(err)
	}
	if got := load(t, s, name); got.Current != "a" {
		t.Fatalf("after seed: %+v", got)
	}

	// A later seed must not overwrite an existing current (preserves history).
	if err := s.Seed(name, "z"); err != nil {
		t.Fatal(err)
	}
	if got := load(t, s, name); got.Current != "a" {
		t.Fatalf("seed overwrote current: %+v", got)
	}
}

func TestSeedEmptyIgnored(t *testing.T) {
	s := newStore(t)
	if err := s.Seed(WorkspaceFile(), ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.dir, WorkspaceFile())); !os.IsNotExist(err) {
		t.Errorf("empty Seed created a file")
	}
}

func TestRingMostRecentFirstDedupedAndCapped(t *testing.T) {
	s := newStore(t)
	name := WorkspaceFile()

	// Push more than the cap, revisiting some ids.
	for i := 0; i < ringCap+50; i++ {
		id := "id-" + itoa(i)
		if _, err := s.Update(name, id); err != nil {
			t.Fatal(err)
		}
	}
	// Revisit an existing id; it should move to the front without duplication.
	if _, err := s.Update(name, "id-"+itoa(ringCap)); err != nil {
		t.Fatal(err)
	}
	rec := load(t, s, name)
	if len(rec.Ring) > ringCap {
		t.Fatalf("ring exceeded cap: %d", len(rec.Ring))
	}
	if rec.Ring[0] != rec.Current {
		t.Errorf("ring[0]=%q, want current %q", rec.Ring[0], rec.Current)
	}
	seen := map[string]bool{}
	for _, id := range rec.Ring {
		if seen[id] {
			t.Fatalf("duplicate %q in ring", id)
		}
		seen[id] = true
	}
}

func TestSaveIsAtomicNoLeftoverTemp(t *testing.T) {
	s := newStore(t)
	name := WorkspaceFile()
	if _, err := s.Update(name, "a"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != name {
			t.Errorf("unexpected leftover file %q", e.Name())
		}
	}
}

func TestPersistedJSONShape(t *testing.T) {
	s := newStore(t)
	name := WorkspaceFile()
	if _, err := s.Update(name, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(name, "b"); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(s.dir, name))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("persisted file is not valid JSON: %v", err)
	}
	if _, ok := raw["current"]; !ok {
		t.Error("missing current key")
	}
	if _, ok := raw["previous"]; !ok {
		t.Error("missing previous key")
	}
}

// helpers

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func load(t *testing.T, s *Store, name string) Record {
	t.Helper()
	rec, err := s.Load(name)
	if err != nil {
		t.Fatalf("Load %s: %v", name, err)
	}
	return rec
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(buf[pos:])
}
