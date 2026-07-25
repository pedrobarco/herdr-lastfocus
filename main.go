// Command herdr-lastfocus provides tmux-style last-active toggles for herdr
// panes, tabs, and workspaces, backed by a background daemon that records focus
// events. Usage: herdr-lastfocus <daemon|last-pane|last-tab|last-workspace>.
package main

import (
	"fmt"
	"os"

	"github.com/pedrobarco/herdr-lastfocus/internal/daemon"
	"github.com/pedrobarco/herdr-lastfocus/internal/herdr"
	"github.com/pedrobarco/herdr-lastfocus/internal/history"
)

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
func lastPane() error {
	c, err := herdr.DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()

	// Determine the current focused tab id: prefer the injected env var, else a
	// live snapshot.
	tabID := os.Getenv("HERDR_TAB_ID")
	var focusedPane string
	if tabID == "" {
		snap, err := c.Snapshot()
		if err != nil {
			return err
		}
		tabID = snap.FocusedTabID
		focusedPane = snap.FocusedPaneID
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
	if rec.Previous == "" {
		return nil
	}

	// If we did not learn the live focused pane from a snapshot above, fetch it
	// so we avoid focusing the pane already focused.
	if focusedPane == "" {
		if snap, err := c.Snapshot(); err == nil {
			focusedPane = snap.FocusedPaneID
		}
	}
	if rec.Previous == focusedPane {
		return nil
	}
	return c.FocusPane(rec.Previous)
}

// lastTab focuses the previously active tab within the current workspace.
func lastTab() error {
	c, err := herdr.DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()

	wsID := os.Getenv("HERDR_WORKSPACE_ID")
	if wsID == "" {
		snap, err := c.Snapshot()
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
	return c.FocusTab(rec.Previous)
}

// lastWorkspace focuses the previously active workspace.
func lastWorkspace() error {
	c, err := herdr.DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()

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
	return c.FocusWorkspace(rec.Previous)
}
