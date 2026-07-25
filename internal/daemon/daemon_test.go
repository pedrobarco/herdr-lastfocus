package daemon

import (
	"testing"

	"github.com/pedrobarco/herdr-lastfocus/internal/herdr"
	"github.com/pedrobarco/herdr-lastfocus/internal/history"
)

// ev builds a focus event of the given type with the supplied data.
func ev(name, ws, tab, pane string) *herdr.Event {
	return &herdr.Event{
		Event: name,
		Data: herdr.EventData{
			Type:        name,
			WorkspaceID: ws,
			TabID:       tab,
			PaneID:      pane,
		},
	}
}

func newTestStore(t *testing.T) *history.Store {
	t.Helper()
	s, err := history.New(t.TempDir())
	if err != nil {
		t.Fatalf("history.New: %v", err)
	}
	return s
}

// TestSameTabPaneSwitchDoesNotPolluteTabOrWorkspace verifies the trio dedupe:
// focusing a different pane within the same tab emits all three events, but only
// the pane scope should record a transition.
func TestSameTabPaneSwitchDoesNotPolluteTabOrWorkspace(t *testing.T) {
	store := newTestStore(t)
	const ws, tab = "w1", "w1:t1"

	// Seed the starting focus (pane p1 in tab t1, workspace w1).
	if err := store.Seed(history.WorkspaceFile(), ws); err != nil {
		t.Fatal(err)
	}
	if err := store.Seed(history.TabFile(ws), tab); err != nil {
		t.Fatal(err)
	}
	if err := store.Seed(history.PaneFile(tab), "w1:p1"); err != nil {
		t.Fatal(err)
	}

	// A same-tab pane switch to p2 arrives as a trio.
	cur := tab
	cur = handleEvent(store, ev("workspace_focused", ws, "", ""), cur, nil)
	cur = handleEvent(store, ev("tab_focused", ws, tab, ""), cur, nil)
	cur = handleEvent(store, ev("pane_focused", ws, "", "w1:p2"), cur, nil)

	if cur != tab {
		t.Fatalf("current tab changed to %q, want %q", cur, tab)
	}

	// Pane scope shifts p1 -> previous, p2 -> current.
	pane, _ := store.Load(history.PaneFile(tab))
	if pane.Current != "w1:p2" || pane.Previous != "w1:p1" {
		t.Errorf("pane record = %+v, want current=w1:p2 previous=w1:p1", pane)
	}

	// Tab scope must be untouched (no previous recorded).
	tabRec, _ := store.Load(history.TabFile(ws))
	if tabRec.Current != tab || tabRec.Previous != "" {
		t.Errorf("tab record polluted: %+v", tabRec)
	}

	// Workspace scope must be untouched.
	wsRec, _ := store.Load(history.WorkspaceFile())
	if wsRec.Current != ws || wsRec.Previous != "" {
		t.Errorf("workspace record polluted: %+v", wsRec)
	}
}

// TestTabSwitchRecordsTabAndPaneButNotWorkspace verifies a cross-tab switch in
// the same workspace records tab and pane history but leaves workspace alone.
func TestTabSwitchRecordsTabAndPaneButNotWorkspace(t *testing.T) {
	store := newTestStore(t)
	const ws = "w1"
	const tab1, tab2 = "w1:t1", "w1:t2"

	store.Seed(history.WorkspaceFile(), ws)
	store.Seed(history.TabFile(ws), tab1)
	store.Seed(history.PaneFile(tab1), "w1:p1")

	// Switch to tab2 / pane p3 (trio). tab_focused carries the new tab id, so
	// the pane event that follows must key against tab2.
	cur := tab1
	cur = handleEvent(store, ev("workspace_focused", ws, "", ""), cur, nil)
	cur = handleEvent(store, ev("tab_focused", ws, tab2, ""), cur, nil)
	cur = handleEvent(store, ev("pane_focused", ws, "", "w1:p3"), cur, nil)

	if cur != tab2 {
		t.Fatalf("current tab = %q, want %q", cur, tab2)
	}

	tabRec, _ := store.Load(history.TabFile(ws))
	if tabRec.Current != tab2 || tabRec.Previous != tab1 {
		t.Errorf("tab record = %+v, want current=%s previous=%s", tabRec, tab2, tab1)
	}

	// Pane history for the new tab is seeded from the trio's pane event.
	paneRec, _ := store.Load(history.PaneFile(tab2))
	if paneRec.Current != "w1:p3" {
		t.Errorf("pane(tab2) current = %q, want w1:p3", paneRec.Current)
	}
	// Original tab's pane history is untouched.
	paneRec1, _ := store.Load(history.PaneFile(tab1))
	if paneRec1.Current != "w1:p1" || paneRec1.Previous != "" {
		t.Errorf("pane(tab1) polluted: %+v", paneRec1)
	}

	wsRec, _ := store.Load(history.WorkspaceFile())
	if wsRec.Previous != "" {
		t.Errorf("workspace polluted on same-workspace tab switch: %+v", wsRec)
	}
}

// TestWorkspaceSwitchRecordsWorkspace verifies a workspace change records the
// workspace transition and updates the current tab tracker.
func TestWorkspaceSwitchRecordsWorkspace(t *testing.T) {
	store := newTestStore(t)
	store.Seed(history.WorkspaceFile(), "w1")
	store.Seed(history.TabFile("w1"), "w1:t1")

	cur := "w1:t1"
	cur = handleEvent(store, ev("workspace_focused", "w2", "", ""), cur, nil)
	cur = handleEvent(store, ev("tab_focused", "w2", "w2:t1", ""), cur, nil)
	cur = handleEvent(store, ev("pane_focused", "w2", "", "w2:p1"), cur, nil)

	if cur != "w2:t1" {
		t.Fatalf("current tab = %q, want w2:t1", cur)
	}

	wsRec, _ := store.Load(history.WorkspaceFile())
	if wsRec.Current != "w2" || wsRec.Previous != "w1" {
		t.Errorf("workspace record = %+v, want current=w2 previous=w1", wsRec)
	}
}

// TestPaneEventWithEmptyDataIgnored ensures malformed/empty events are no-ops.
func TestPaneEventWithEmptyDataIgnored(t *testing.T) {
	store := newTestStore(t)
	cur := handleEvent(store, ev("pane_focused", "", "", ""), "w1:t1", nil)
	if cur != "w1:t1" {
		t.Errorf("current tab changed on empty pane event: %q", cur)
	}
	rec, _ := store.Load(history.PaneFile("w1:t1"))
	if rec.Current != "" || rec.Previous != "" || len(rec.Ring) != 0 {
		t.Errorf("empty pane event recorded: %+v", rec)
	}
}
