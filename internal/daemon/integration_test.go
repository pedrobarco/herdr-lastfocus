package daemon

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/pedrobarco/herdr-lastfocus/internal/history"
)

// fakeHerdr is a minimal herdr server on a real unix domain socket. It answers
// session.snapshot and events.subscribe, then pushes event lines on demand,
// exercising the daemon's real Dial/snapshot/subscribe/read code paths.
type fakeHerdr struct {
	t        *testing.T
	ln       net.Listener
	path     string
	snapshot map[string]any

	mu      sync.Mutex
	conn    net.Conn       // the active client connection (for pushing events)
	current map[string]any // pane returned by pane.current; nil answers empty

	subscribed chan struct{} // closed-style signal via send when subscribe seen
	wg         sync.WaitGroup
}

// newFakeHerdr starts a fake server. The socket lives under a short /tmp dir to
// stay within the macOS ~104-byte sun_path limit (t.TempDir can be too long).
func newFakeHerdr(t *testing.T, snapshot map[string]any) *fakeHerdr {
	t.Helper()

	dir, err := os.MkdirTemp("/tmp", "lf-sock-")
	if err != nil {
		t.Fatalf("mkdir sock dir: %v", err)
	}
	sockPath := filepath.Join(dir, "herdr.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	f := &fakeHerdr{
		t:          t,
		ln:         ln,
		path:       sockPath,
		snapshot:   snapshot,
		subscribed: make(chan struct{}, 1),
	}

	t.Cleanup(func() {
		f.ln.Close()
		f.mu.Lock()
		if f.conn != nil {
			f.conn.Close()
		}
		f.mu.Unlock()
		f.wg.Wait()
		os.RemoveAll(dir)
	})

	f.wg.Add(1)
	go f.serve()
	return f
}

// serve accepts client connections and handles each concurrently. Like the real
// server, snapshots and subscriptions arrive on separate connections; the
// connection that subscribes becomes the one events are pushed to.
func (f *fakeHerdr) serve() {
	defer f.wg.Done()
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return // listener closed
		}
		f.wg.Add(1)
		go f.handleConn(conn)
	}
}

// handleConn processes request lines on a single connection.
func (f *fakeHerdr) handleConn(conn net.Conn) {
	defer f.wg.Done()
	defer conn.Close()

	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req struct {
			ID     string `json:"id"`
			Method string `json:"method"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}
		switch req.Method {
		case "session.snapshot":
			f.reply(conn, req.ID, map[string]any{"snapshot": f.snapshot})
		case "events.subscribe":
			// This connection receives pushed events.
			f.mu.Lock()
			f.conn = conn
			f.mu.Unlock()
			f.reply(conn, req.ID, map[string]any{"type": "subscription_started"})
			select {
			case f.subscribed <- struct{}{}:
			default:
			}
		case "pane.current":
			f.mu.Lock()
			cur := f.current
			f.mu.Unlock()
			f.reply(conn, req.ID, map[string]any{"pane": cur})
		default:
			// Focus calls and anything else: acknowledge with an empty result.
			f.reply(conn, req.ID, map[string]any{})
		}
	}
}

// reply writes a success response line for the given request id.
func (f *fakeHerdr) reply(conn net.Conn, id string, result map[string]any) {
	resp := map[string]any{"id": id, "result": result}
	b, err := json.Marshal(resp)
	if err != nil {
		f.t.Errorf("marshal reply: %v", err)
		return
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return
	}
}

// waitSubscribed blocks until the client has subscribed (or times out).
func (f *fakeHerdr) waitSubscribed() {
	select {
	case <-f.subscribed:
	case <-time.After(2 * time.Second):
		f.t.Fatal("timed out waiting for subscribe")
	}
}

// pushEvent writes one event line to the connected client.
func (f *fakeHerdr) pushEvent(name string, data map[string]any) {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn == nil {
		f.t.Fatal("pushEvent before client connected")
	}
	ev := map[string]any{"event": name, "data": data}
	b, err := json.Marshal(ev)
	if err != nil {
		f.t.Errorf("marshal event: %v", err)
		return
	}
	if _, err := conn.Write(append(b, '\n')); err != nil {
		f.t.Errorf("push event: %v", err)
	}
}

// pushPaneTrio emits the trio herdr sends on a pane focus, in the real server
// order: pane_focused, then tab_focused, then workspace_focused. pane_focused
// intentionally omits tab_id.
func (f *fakeHerdr) pushPaneTrio(ws, tab, pane string) {
	f.pushEvent("pane_focused", map[string]any{"type": "pane_focused", "pane_id": pane, "workspace_id": ws})
	f.pushEvent("tab_focused", map[string]any{"type": "tab_focused", "tab_id": tab, "workspace_id": ws})
	f.pushEvent("workspace_focused", map[string]any{"type": "workspace_focused", "workspace_id": ws})
}

// setCurrent sets the pane that pane.current reports, which simulates a
// keyboard focus change that herdr does not publish as an event.
func (f *fakeHerdr) setCurrent(ws, tab, pane string) {
	f.mu.Lock()
	f.current = map[string]any{"pane_id": pane, "tab_id": tab, "workspace_id": ws}
	f.mu.Unlock()
}

// closeClient closes the server side of the client connection, ending the
// daemon's session (NextEvent returns an error).
func (f *fakeHerdr) closeClient() {
	f.mu.Lock()
	conn := f.conn
	f.mu.Unlock()
	if conn != nil {
		conn.Close()
	}
}

// runSession runs the daemon's real session() against the fake server in a
// goroutine and returns a channel that receives its error when the stream ends.
func runSession(t *testing.T, store *history.Store) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- session(store) }()
	return done
}

// eventually polls until cond is true or the deadline passes.
func eventually(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met before deadline")
}

// TestSessionSeedsFromSnapshot verifies the daemon seeds each scope's current id
// from the initial snapshot over a real socket.
func TestSessionSeedsFromSnapshot(t *testing.T) {
	f := newFakeHerdr(t, map[string]any{
		"focused_workspace_id": "w1",
		"focused_tab_id":       "w1:t1",
		"focused_pane_id":      "w1:p1",
		"panes": []map[string]any{
			{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "focused": true},
		},
	})
	t.Setenv("HERDR_SOCKET_PATH", f.path)

	dir := shortStateDir(t)
	store, err := history.New(dir)
	if err != nil {
		t.Fatal(err)
	}

	done := runSession(t, store)
	f.waitSubscribed()

	eventually(t, func() bool {
		ws, _ := store.Load(history.WorkspaceFile())
		tab, _ := store.Load(history.TabFile("w1"))
		pane, _ := store.Load(history.PaneFile("w1:t1"))
		return ws.Current == "w1" && tab.Current == "w1:t1" && pane.Current == "w1:p1"
	})

	f.closeClient()
	<-done
}

// TestSessionSameTabPaneSwitchOverSocket drives a full same-tab pane switch trio
// through the real socket and asserts only pane history changed.
func TestSessionSameTabPaneSwitchOverSocket(t *testing.T) {
	f := newFakeHerdr(t, map[string]any{
		"focused_workspace_id": "w1",
		"focused_tab_id":       "w1:t1",
		"focused_pane_id":      "w1:p1",
		"panes": []map[string]any{
			{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "focused": true},
			{"pane_id": "w1:p2", "tab_id": "w1:t1", "workspace_id": "w1"},
		},
	})
	t.Setenv("HERDR_SOCKET_PATH", f.path)

	store, err := history.New(shortStateDir(t))
	if err != nil {
		t.Fatal(err)
	}

	done := runSession(t, store)
	f.waitSubscribed()

	// Same-tab pane switch p1 -> p2.
	f.pushPaneTrio("w1", "w1:t1", "w1:p2")

	eventually(t, func() bool {
		pane, _ := store.Load(history.PaneFile("w1:t1"))
		return pane.Current == "w1:p2" && pane.Previous == "w1:p1"
	})

	// Tab and workspace scopes must not have gained a previous.
	tab, _ := store.Load(history.TabFile("w1"))
	if tab.Previous != "" {
		t.Errorf("tab history polluted by same-tab pane switch: %+v", tab)
	}
	ws, _ := store.Load(history.WorkspaceFile())
	if ws.Previous != "" {
		t.Errorf("workspace history polluted by same-tab pane switch: %+v", ws)
	}

	f.closeClient()
	<-done
}

// TestSessionTabSwitchOverSocket drives a cross-tab switch and asserts tab and
// pane history update while workspace stays put.
func TestSessionTabSwitchOverSocket(t *testing.T) {
	f := newFakeHerdr(t, map[string]any{
		"focused_workspace_id": "w1",
		"focused_tab_id":       "w1:t1",
		"focused_pane_id":      "w1:p1",
		"panes": []map[string]any{
			{"pane_id": "w1:p1", "tab_id": "w1:t1", "workspace_id": "w1", "focused": true},
		},
	})
	t.Setenv("HERDR_SOCKET_PATH", f.path)

	store, err := history.New(shortStateDir(t))
	if err != nil {
		t.Fatal(err)
	}

	done := runSession(t, store)
	f.waitSubscribed()

	// Switch to tab t2 / pane p3 within the same workspace.
	f.pushPaneTrio("w1", "w1:t2", "w1:p3")

	eventually(t, func() bool {
		tab, _ := store.Load(history.TabFile("w1"))
		return tab.Current == "w1:t2" && tab.Previous == "w1:t1"
	})

	// New tab's pane history seeded from the trio.
	eventually(t, func() bool {
		pane2, _ := store.Load(history.PaneFile("w1:t2"))
		return pane2.Current == "w1:p3"
	})

	// Workspace unchanged.
	ws, _ := store.Load(history.WorkspaceFile())
	if ws.Previous != "" {
		t.Errorf("workspace polluted on same-workspace tab switch: %+v", ws)
	}

	f.closeClient()
	<-done
}

// TestSessionWorkspaceSwitchOverSocket drives a workspace change end to end.
func TestSessionWorkspaceSwitchOverSocket(t *testing.T) {
	f := newFakeHerdr(t, map[string]any{
		"focused_workspace_id": "w1",
		"focused_tab_id":       "w1:t1",
		"focused_pane_id":      "w1:p1",
	})
	t.Setenv("HERDR_SOCKET_PATH", f.path)

	store, err := history.New(shortStateDir(t))
	if err != nil {
		t.Fatal(err)
	}

	done := runSession(t, store)
	f.waitSubscribed()

	f.pushPaneTrio("w2", "w2:t1", "w2:p1")

	eventually(t, func() bool {
		ws, _ := store.Load(history.WorkspaceFile())
		return ws.Current == "w2" && ws.Previous == "w1"
	})

	f.closeClient()
	<-done
}

// TestSessionToggleSequenceOverSocket drives an A->B->A->B tab toggle and
// asserts the final current/previous form a stable A<->B pair.
func TestSessionToggleSequenceOverSocket(t *testing.T) {
	f := newFakeHerdr(t, map[string]any{
		"focused_workspace_id": "w1",
		"focused_tab_id":       "w1:t1",
		"focused_pane_id":      "w1:p1",
	})
	t.Setenv("HERDR_SOCKET_PATH", f.path)

	store, err := history.New(shortStateDir(t))
	if err != nil {
		t.Fatal(err)
	}

	done := runSession(t, store)
	f.waitSubscribed()

	for _, tab := range []string{"w1:t2", "w1:t1", "w1:t2"} {
		f.pushEvent("tab_focused", map[string]any{
			"type": "tab_focused", "tab_id": tab, "workspace_id": "w1",
		})
	}

	eventually(t, func() bool {
		tab, _ := store.Load(history.TabFile("w1"))
		return tab.Current == "w1:t2" && tab.Previous == "w1:t1"
	})

	f.closeClient()
	<-done
}

// shortStateDir returns a state dir under /tmp to keep paths short and isolated.
func shortStateDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "lf-state-")
	if err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// TestPollRecordsKeyboardTabSwitches verifies that the pane.current poll
// records tab and workspace history, because herdr 0.9.0 fires no focus
// events for keyboard navigation. Without this, last-tab only ever sees its
// own jumps and toggles one fixed pair of tabs.
func TestPollRecordsKeyboardTabSwitches(t *testing.T) {
	f := newFakeHerdr(t, map[string]any{})
	t.Setenv("HERDR_SOCKET_PATH", f.path)

	store, err := history.New(shortStateDir(t))
	if err != nil {
		t.Fatal(err)
	}
	r := newRecorder(store)

	for _, step := range []struct{ ws, tab, pane string }{
		{"w1", "w1:t1", "w1:p1"},
		{"w1", "w1:t2", "w1:p2"},
		{"w1", "w1:t3", "w1:p3"},
		{"w2", "w2:t1", "w2:p1"},
	} {
		f.setCurrent(step.ws, step.tab, step.pane)
		r.pollCurrentPane()
	}

	tab, _ := store.Load(history.TabFile("w1"))
	if tab.Current != "w1:t3" || tab.Previous != "w1:t2" {
		t.Errorf("w1 tab record = %+v, want current=w1:t3 previous=w1:t2", tab)
	}
	ws, _ := store.Load(history.WorkspaceFile())
	if ws.Current != "w2" || ws.Previous != "w1" {
		t.Errorf("workspace record = %+v, want current=w2 previous=w1", ws)
	}
}
