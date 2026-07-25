// Package daemon implements the background focus-event recorder. It connects to
// the herdr socket, subscribes to focus events, and persists per-scope history
// so the toggle actions can jump to the last-active pane/tab/workspace.
package daemon

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/pedrobarco/herdr-lastfocus/internal/herdr"
	"github.com/pedrobarco/herdr-lastfocus/internal/history"
)

const (
	pidFileName  = "daemon.pid"
	lockFileName = "daemon.lock"
	logFileName  = "daemon.log"

	// runFlag marks the detached child process that runs the actual loop.
	runFlag = "--run"
	// ensureFlag is the idempotent launcher invoked by the startup hook.
	ensureFlag = "--ensure"
)

// StateDir returns the plugin state directory from the environment.
func StateDir() (string, error) {
	d := os.Getenv("HERDR_PLUGIN_STATE_DIR")
	if d == "" {
		return "", fmt.Errorf("HERDR_PLUGIN_STATE_DIR is not set")
	}
	if err := os.MkdirAll(d, 0o755); err != nil {
		return "", err
	}
	return d, nil
}

// Dispatch handles the "daemon" subcommand given its arguments (after "daemon").
func Dispatch(args []string) error {
	mode := ensureFlag
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case ensureFlag:
		return Ensure()
	case runFlag:
		return runDetached()
	default:
		return fmt.Errorf("unknown daemon mode %q", mode)
	}
}

// Ensure idempotently launches the daemon. If a live daemon already holds the
// lock it returns nil (exit 0). Otherwise it spawns a detached child running the
// loop and returns.
func Ensure() error {
	dir, err := StateDir()
	if err != nil {
		return err
	}

	// Acquire the lock non-blocking to test liveness. If another live process
	// holds it, we are done. We release immediately so the child can take it.
	lockPath := filepath.Join(dir, lockFileName)
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Lock held by a live daemon.
		lf.Close()
		return nil
	}
	// We got the lock, meaning no live daemon. Release it and clean stale pid,
	// then spawn the detached child which will re-acquire it.
	syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
	lf.Close()
	removeStalePID(dir)

	return spawnDetached(dir)
}

// spawnDetached re-execs this binary as a session leader with detached stdio so
// it survives the startup hook process exiting.
func spawnDetached(dir string) error {
	self, err := os.Executable()
	if err != nil {
		return err
	}
	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer devnull.Close()

	logPath := filepath.Join(dir, logFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer logFile.Close()

	cmd := exec.Command(self, "daemon", runFlag)
	cmd.Env = os.Environ()
	cmd.Stdin = devnull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	// Do not Wait; let it run independently.
	return nil
}

// removeStalePID deletes a pid file whose process is no longer alive.
func removeStalePID(dir string) {
	pidPath := filepath.Join(dir, pidFileName)
	b, err := os.ReadFile(pidPath)
	if err != nil {
		return
	}
	pid, err := strconv.Atoi(string(trimSpace(b)))
	if err != nil {
		os.Remove(pidPath)
		return
	}
	if !processAlive(pid) {
		os.Remove(pidPath)
	}
}

// trimSpace trims surrounding ASCII whitespace without importing strings.
func trimSpace(b []byte) []byte {
	start := 0
	for start < len(b) && isSpace(b[start]) {
		start++
	}
	end := len(b)
	for end > start && isSpace(b[end-1]) {
		end--
	}
	return b[start:end]
}

func isSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}

// processAlive reports whether a process with pid exists.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return proc.Signal(syscall.Signal(0)) == nil
}

// runDetached runs inside the detached child: acquire the lock, write the pid,
// and run the recording loop until the socket is gone.
func runDetached() error {
	dir, err := StateDir()
	if err != nil {
		return err
	}

	logPath := filepath.Join(dir, logFileName)
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		log.SetOutput(logFile)
		defer logFile.Close()
	}
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[lastfocus] ")

	lockPath := filepath.Join(dir, lockFileName)
	lf, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		// Another daemon won the race; exit quietly.
		log.Printf("another daemon holds the lock, exiting")
		return nil
	}
	defer syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)

	pidPath := filepath.Join(dir, pidFileName)
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		return err
	}
	defer os.Remove(pidPath)

	store, err := history.New(dir)
	if err != nil {
		return err
	}

	log.Printf("daemon started pid=%d", os.Getpid())
	err = loop(store)
	log.Printf("daemon exiting: %v", err)
	return err
}

// loop runs the connect/subscribe/read cycle with reconnect backoff. It returns
// when the socket is permanently gone (server stopped).
func loop(store *history.Store) error {
	const (
		minBackoff = 200 * time.Millisecond
		maxBackoff = 5 * time.Second
	)
	backoff := minBackoff

	for {
		// If the socket path is gone, the server has stopped: exit.
		path, err := herdr.SocketPath()
		if err != nil {
			return err
		}
		if _, statErr := os.Stat(path); statErr != nil {
			return fmt.Errorf("socket gone: %w", statErr)
		}

		if err := session(store); err != nil {
			if errors.Is(err, io.EOF) {
				log.Printf("stream closed, reconnecting")
			} else {
				log.Printf("session error: %v", err)
			}
		}

		// Reconnect with backoff; give up only if the socket has disappeared.
		if _, statErr := os.Stat(path); statErr != nil {
			return fmt.Errorf("socket gone: %w", statErr)
		}
		time.Sleep(backoff)
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}

// session opens one connection, seeds scopes from a snapshot, subscribes, and
// records events until the stream closes.
func session(store *history.Store) error {
	c, err := herdr.DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()

	snap, err := c.Snapshot()
	if err != nil {
		return err
	}

	// Track the current focused tab id continuously so pane events (which lack
	// tab_id) can be keyed per tab.
	currentTab := snap.FocusedTabID

	// Seed each scope's current id (only when unset, preserving history).
	if snap.FocusedWorkspaceID != "" {
		if err := store.Seed(history.WorkspaceFile(), snap.FocusedWorkspaceID); err != nil {
			log.Printf("seed workspace: %v", err)
		}
	}
	if snap.FocusedTabID != "" && snap.FocusedWorkspaceID != "" {
		if err := store.Seed(history.TabFile(snap.FocusedWorkspaceID), snap.FocusedTabID); err != nil {
			log.Printf("seed tab: %v", err)
		}
	}
	if snap.FocusedPaneID != "" && currentTab != "" {
		if err := store.Seed(history.PaneFile(currentTab), snap.FocusedPaneID); err != nil {
			log.Printf("seed pane: %v", err)
		}
	}

	subs := []herdr.Subscription{
		{Type: "pane.focused"},
		{Type: "tab.focused"},
		{Type: "workspace.focused"},
	}
	if err := c.Subscribe(subs); err != nil {
		return err
	}
	log.Printf("subscribed; current tab=%s", currentTab)

	for {
		ev, err := c.NextEvent()
		if err != nil {
			return err
		}
		currentTab = handleEvent(store, ev, currentTab, c)
	}
}

// handleEvent applies one focus event and returns the (possibly updated) current
// tab id. Each scope only shifts current->previous when that scope's id actually
// changed, so same-tab pane switches never pollute tab/workspace history.
func handleEvent(store *history.Store, ev *herdr.Event, currentTab string, c *herdr.Client) string {
	d := ev.Data
	switch ev.Event {
	case "workspace_focused":
		if d.WorkspaceID != "" {
			if _, err := store.Update(history.WorkspaceFile(), d.WorkspaceID); err != nil {
				log.Printf("update workspace: %v", err)
			}
		}
	case "tab_focused":
		// tab_focused carries tab_id and workspace_id. Update the current tab
		// tracker first so any pane event in the same trio keys correctly.
		if d.TabID != "" {
			currentTab = d.TabID
		}
		if d.TabID != "" && d.WorkspaceID != "" {
			if _, err := store.Update(history.TabFile(d.WorkspaceID), d.TabID); err != nil {
				log.Printf("update tab: %v", err)
			}
		}
	case "pane_focused":
		// pane_focused lacks tab_id; use the tracked current tab, falling back
		// to a snapshot lookup by pane_id if the tracker is empty.
		tab := currentTab
		if tab == "" && d.PaneID != "" {
			tab = lookupTabForPane(c, d.PaneID)
			if tab != "" {
				currentTab = tab
			}
		}
		if d.PaneID != "" && tab != "" {
			if _, err := store.Update(history.PaneFile(tab), d.PaneID); err != nil {
				log.Printf("update pane: %v", err)
			}
		}
	}
	return currentTab
}

// lookupTabForPane resolves a pane's tab id via a fresh snapshot.
func lookupTabForPane(c *herdr.Client, paneID string) string {
	snap, err := c.Snapshot()
	if err != nil {
		log.Printf("snapshot for pane lookup: %v", err)
		return ""
	}
	for _, p := range snap.Panes {
		if p.PaneID == paneID {
			return p.TabID
		}
	}
	return ""
}
