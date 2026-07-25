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

// trio feeds a focus change in the real server order: pane_focused, then
// tab_focused, then workspace_focused. pane_focused carries no tab_id.
func trio(r *recorder, ws, tab, pane string) {
	r.handle(ev("pane_focused", ws, "", pane))
	r.handle(ev("tab_focused", ws, tab, ""))
	r.handle(ev("workspace_focused", ws, "", ""))
}

// TestSameTabPaneSwitchDoesNotPolluteTabOrWorkspace verifies the trio dedupe:
// focusing a different pane within the same tab still emits all three events,
// but only the pane scope should record a transition.
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

	r := newRecorder(store)
	r.currentTab = tab
	trio(r, ws, tab, "w1:p2")

	if r.currentTab != tab {
		t.Fatalf("current tab changed to %q, want %q", r.currentTab, tab)
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
// The pane event arrives before its tab is known, so the recorder must key it
// against the trailing tab_focused (tab2), not the previously tracked tab1.
func TestTabSwitchRecordsTabAndPaneButNotWorkspace(t *testing.T) {
	store := newTestStore(t)
	const ws = "w1"
	const tab1, tab2 = "w1:t1", "w1:t2"

	store.Seed(history.WorkspaceFile(), ws)
	store.Seed(history.TabFile(ws), tab1)
	store.Seed(history.PaneFile(tab1), "w1:p1")

	r := newRecorder(store)
	r.currentTab = tab1
	trio(r, ws, tab2, "w1:p3")

	if r.currentTab != tab2 {
		t.Fatalf("current tab = %q, want %q", r.currentTab, tab2)
	}

	tabRec, _ := store.Load(history.TabFile(ws))
	if tabRec.Current != tab2 || tabRec.Previous != tab1 {
		t.Errorf("tab record = %+v, want current=%s previous=%s", tabRec, tab2, tab1)
	}

	// Pane history for the new tab is keyed to tab2, not the stale tab1.
	paneRec, _ := store.Load(history.PaneFile(tab2))
	if paneRec.Current != "w1:p3" {
		t.Errorf("pane(tab2) current = %q, want w1:p3", paneRec.Current)
	}
	// Original tab's pane history must not have been polluted.
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
// workspace transition, keys the pane to the new tab, and updates the tracker.
func TestWorkspaceSwitchRecordsWorkspace(t *testing.T) {
	store := newTestStore(t)
	store.Seed(history.WorkspaceFile(), "w1")
	store.Seed(history.TabFile("w1"), "w1:t1")
	store.Seed(history.PaneFile("w1:t1"), "w1:p1")

	r := newRecorder(store)
	r.currentTab = "w1:t1"
	trio(r, "w2", "w2:t1", "w2:p1")

	if r.currentTab != "w2:t1" {
		t.Fatalf("current tab = %q, want w2:t1", r.currentTab)
	}

	wsRec, _ := store.Load(history.WorkspaceFile())
	if wsRec.Current != "w2" || wsRec.Previous != "w1" {
		t.Errorf("workspace record = %+v, want current=w2 previous=w1", wsRec)
	}

	// The w2 pane must be keyed to w2:t1, never leaking into w1:t1.
	pane2, _ := store.Load(history.PaneFile("w2:t1"))
	if pane2.Current != "w2:p1" {
		t.Errorf("pane(w2:t1) current = %q, want w2:p1", pane2.Current)
	}
	pane1, _ := store.Load(history.PaneFile("w1:t1"))
	if pane1.Current != "w1:p1" || pane1.Previous != "" {
		t.Errorf("pane(w1:t1) polluted by cross-workspace switch: %+v", pane1)
	}
}

// TestPaneBeforeTabKeysToNewTab is the core regression: because pane_focused
// arrives before tab_focused, a naive tracker would key the pane to the old tab.
// The recorder must instead key it to the tab_focused that follows.
func TestPaneBeforeTabKeysToNewTab(t *testing.T) {
	store := newTestStore(t)
	r := newRecorder(store)
	r.currentTab = "w1:t1"

	// Cross-workspace pane event arrives first, then its tab.
	r.handle(ev("pane_focused", "w3", "", "w3:p1"))
	// At this point nothing should be written yet (pane is pending).
	if rec, _ := store.Load(history.PaneFile("w1:t1")); rec.Current != "" {
		t.Fatalf("pending pane leaked into w1:t1: %+v", rec)
	}
	r.handle(ev("tab_focused", "w3", "w3:t1", ""))

	// The pane must land in w3:t1, not w1:t1.
	if rec, _ := store.Load(history.PaneFile("w3:t1")); rec.Current != "w3:p1" {
		t.Errorf("pane(w3:t1) = %+v, want current=w3:p1", rec)
	}
	if rec, _ := store.Load(history.PaneFile("w1:t1")); rec.Current != "" {
		t.Errorf("pane leaked into w1:t1: %+v", rec)
	}
}

// TestLonePaneSwitchFlushesOnWorkspace verifies that a pane event followed by a
// workspace event (no intervening tab_focused) is flushed against the tracked
// current tab.
func TestLonePaneSwitchFlushesOnWorkspace(t *testing.T) {
	store := newTestStore(t)
	store.Seed(history.PaneFile("w1:t1"), "w1:p1")

	r := newRecorder(store)
	r.currentTab = "w1:t1"

	r.handle(ev("pane_focused", "w1", "", "w1:p2"))
	r.handle(ev("workspace_focused", "w1", "", ""))

	rec, _ := store.Load(history.PaneFile("w1:t1"))
	if rec.Current != "w1:p2" || rec.Previous != "w1:p1" {
		t.Errorf("pane record = %+v, want current=w1:p2 previous=w1:p1", rec)
	}
}

// TestConsecutivePaneEventsFlushPrevious verifies that a second pane event
// flushes the first pending one (via the tracked tab) before buffering itself.
func TestConsecutivePaneEventsFlushPrevious(t *testing.T) {
	store := newTestStore(t)
	r := newRecorder(store)
	r.currentTab = "w1:t1"

	r.handle(ev("pane_focused", "w1", "", "w1:p1"))
	r.handle(ev("pane_focused", "w1", "", "w1:p2"))
	// Resolve the second via its tab.
	r.handle(ev("tab_focused", "w1", "w1:t1", ""))

	rec, _ := store.Load(history.PaneFile("w1:t1"))
	if rec.Current != "w1:p2" || rec.Previous != "w1:p1" {
		t.Errorf("pane record = %+v, want current=w1:p2 previous=w1:p1", rec)
	}
}

// TestEmptyPaneEventIgnored ensures empty pane events do not buffer or record.
func TestEmptyPaneEventIgnored(t *testing.T) {
	store := newTestStore(t)
	r := newRecorder(store)
	r.currentTab = "w1:t1"

	r.handle(ev("pane_focused", "", "", ""))
	if r.pending {
		t.Error("empty pane event buffered a pending pane")
	}
	r.handle(ev("tab_focused", "w1", "w1:t1", ""))

	rec, _ := store.Load(history.PaneFile("w1:t1"))
	if rec.Current != "" || rec.Previous != "" || len(rec.Ring) != 0 {
		t.Errorf("empty pane event recorded: %+v", rec)
	}
}

// TestWorkspaceOfTab covers the ws:tab prefix helper.
func TestWorkspaceOfTab(t *testing.T) {
	cases := map[string]string{
		"w1:t1":    "w1",
		"w12:t3":   "w12",
		"notabid":  "",
		":leading": "",
		"w1:t1:x":  "w1",
		"":         "",
	}
	for in, want := range cases {
		if got := workspaceOfTab(in); got != want {
			t.Errorf("workspaceOfTab(%q) = %q, want %q", in, got, want)
		}
	}
}
