// Command herdr-lastfocus provides tmux-style last-active toggles for herdr
// panes, tabs, and workspaces, backed by a background daemon that records focus
// events. Usage: herdr-lastfocus <daemon|last-pane|last-tab|last-workspace>.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/pedrobarco/herdr-lastfocus/internal/daemon"
	"github.com/pedrobarco/herdr-lastfocus/internal/herdr"
	"github.com/pedrobarco/herdr-lastfocus/internal/history"
)

// togglePair is the persisted active two-pane toggle context for a tab.
// Last is the pane id that was most recently focused BY a toggle action.
// The pair is only consulted when the current pane equals Last, meaning
// the user arrived here via toggle. If they arrived via manual navigation,
// the ring walk takes precedence and re-establishes a fresh pair.
type togglePair struct {
	A    string `json:"a"`
	B    string `json:"b"`
	Last string `json:"last"`
}

func toggleFile(tabID string) string { return "toggle." + tabID + ".json" }

func loadTogglePair(dir, tabID string) togglePair {
	b, err := os.ReadFile(filepath.Join(dir, toggleFile(tabID)))
	if err != nil {
		return togglePair{}
	}
	var p togglePair
	_ = json.Unmarshal(b, &p)
	return p
}

func saveTogglePair(dir, tabID, a, b string) {
	data, err := json.Marshal(togglePair{A: a, B: b, Last: b})
	if err != nil {
		return
	}
	dst := filepath.Join(dir, toggleFile(tabID))
	tmp, err := os.CreateTemp(dir, "toggle.tmp-*")
	if err != nil {
		return
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return
	}
	tmp.Close()
	os.Rename(name, dst)
}


func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd := os.Args[1]
	args := os.Args[2:]

	var err error
	switch cmd {
	case "daemon":
		err = daemon.Dispatch(args)
	case "last-pane":
		err = lastPane()
	case "last-tab":
		err = lastTab()
	case "last-workspace":
		err = lastWorkspace()
	case "-h", "--help", "help":
		usage()
		return
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n", cmd)
		usage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "herdr-lastfocus %s: %v\n", cmd, err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: herdr-lastfocus <daemon|last-pane|last-tab|last-workspace>")
}

// stateStore opens the history store rooted at the plugin state dir.
func stateStore() (*history.Store, error) {
	dir, err := daemon.StateDir()
	if err != nil {
		return nil, err
	}
	return history.New(dir)
}

// lastPane focuses the previously active pane within the current tab.
// It walks the history ring most-recent-first and focuses the first entry
// that (a) is not the currently focused pane and (b) still exists in herdr.
// This handles stale daemon state, deleted panes, and A<->B toggle cleanly.
func lastPane() error {
	// One snapshot gives the focused pane and the live pane list.
	snap, err := herdr.SnapshotOnce()
	if err != nil {
		return err
	}

	tabID := os.Getenv("HERDR_TAB_ID")
	if tabID == "" {
		tabID = snap.FocusedTabID
	}
	if tabID == "" {
		return nil
	}

	store, err := stateStore()
	if err != nil {
		return err
	}
	rec, err := store.Load(history.PaneFile(tabID))
	if err != nil {
		return err
	}

	focusedPane := snap.FocusedPaneID

	// Index panes that currently exist in this tab.
	tabPanes := make(map[string]bool, len(snap.Panes))
	for _, p := range snap.Panes {
		if p.TabID == tabID {
			tabPanes[p.PaneID] = true
		}
	}

	// The pair is only valid when the current pane was focused BY a prior toggle
	// action (pair.Last == focusedPane). If the user arrived here via manual
	// navigation the ring walk takes precedence and re-establishes a fresh pair.
	pair := loadTogglePair(store.Dir(), tabID)
	target := ""
	if focusedPane == pair.Last {
		if focusedPane == pair.A && pair.B != "" && tabPanes[pair.B] {
			target = pair.B
		} else if focusedPane == pair.B && pair.A != "" && tabPanes[pair.A] {
			target = pair.A
		}
	}
	// Fall back to ring walk when pair does not apply or the paired pane is gone.
	if target == "" {
		for _, id := range rec.Ring {
			if id != focusedPane && tabPanes[id] {
				target = id
				break
			}
		}
	}
	if target == "" {
		return nil
	}
	// Always persist the new pair so the next toggle press can use it.
	// Last is set to target, marking it as the pane we just focused via toggle.
	saveTogglePair(store.Dir(), tabID, focusedPane, target)
	return herdr.FocusPaneOnce(target)
}

// lastTab focuses the previously active tab within the current workspace.
func lastTab() error {
	wsID := os.Getenv("HERDR_WORKSPACE_ID")
	if wsID == "" {
		snap, err := herdr.SnapshotOnce()
		if err != nil {
			return err
		}
		wsID = snap.FocusedWorkspaceID
	}
	if wsID == "" {
		return nil
	}

	store, err := stateStore()
	if err != nil {
		return err
	}
	rec, err := store.Load(history.TabFile(wsID))
	if err != nil {
		return err
	}
	if rec.Previous == "" {
		return nil
	}
	return herdr.FocusTabOnce(rec.Previous)
}

// lastWorkspace focuses the previously active workspace.
func lastWorkspace() error {
	store, err := stateStore()
	if err != nil {
		return err
	}
	rec, err := store.Load(history.WorkspaceFile())
	if err != nil {
		return err
	}
	if rec.Previous == "" {
		return nil
	}
	return herdr.FocusWorkspaceOnce(rec.Previous)
}
