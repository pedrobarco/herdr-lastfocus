package herdr

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newPipeClient returns a Client wired to an in-memory pipe plus the server-side
// end for the fake herdr server to read requests and write responses.
func newPipeClient(t *testing.T) (*Client, net.Conn) {
	t.Helper()
	clientConn, serverConn := net.Pipe()
	c := &Client{
		conn: clientConn,
		r:    bufio.NewReader(clientConn),
		w:    bufio.NewWriter(clientConn),
	}
	t.Cleanup(func() {
		clientConn.Close()
		serverConn.Close()
	})
	return c, serverConn
}

// readRequest reads one request line from the server side.
func readRequest(t *testing.T, r *bufio.Reader) request {
	t.Helper()
	line, err := r.ReadBytes('\n')
	if err != nil {
		t.Fatalf("server read: %v", err)
	}
	var req request
	if err := json.Unmarshal(line, &req); err != nil {
		t.Fatalf("server decode request: %v", err)
	}
	return req
}

func writeLine(t *testing.T, conn net.Conn, v string) {
	t.Helper()
	if _, err := conn.Write([]byte(v + "\n")); err != nil {
		t.Fatalf("server write: %v", err)
	}
}

func TestSnapshotDecodes(t *testing.T) {
	c, server := newPipeClient(t)
	sr := bufio.NewReader(server)

	go func() {
		req := readRequest(t, sr)
		if req.Method != "session.snapshot" {
			t.Errorf("method = %q, want session.snapshot", req.Method)
		}
		resp := `{"id":"` + req.ID + `","result":{"snapshot":{` +
			`"focused_workspace_id":"w1","focused_tab_id":"w1:t1","focused_pane_id":"w1:p1",` +
			`"panes":[{"pane_id":"w1:p1","tab_id":"w1:t1","workspace_id":"w1","focused":true}]}}}`
		writeLine(t, server, resp)
	}()

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.FocusedWorkspaceID != "w1" || snap.FocusedTabID != "w1:t1" || snap.FocusedPaneID != "w1:p1" {
		t.Fatalf("snapshot fields = %+v", snap)
	}
	if len(snap.Panes) != 1 || snap.Panes[0].TabID != "w1:t1" {
		t.Fatalf("panes = %+v", snap.Panes)
	}
}

func TestCallReturnsRPCError(t *testing.T) {
	// The live server encodes error codes as strings (e.g. "invalid_request");
	// older/other messages may use a number. Both must decode.
	cases := []struct {
		name     string
		codeJSON string
		wantCode string
	}{
		{"string code", `"invalid_request"`, `"invalid_request"`},
		{"numeric code", `-1`, `-1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, server := newPipeClient(t)
			sr := bufio.NewReader(server)

			go func() {
				req := readRequest(t, sr)
				writeLine(t, server, `{"id":"`+req.ID+`","error":{"code":`+tc.codeJSON+`,"message":"boom"}}`)
			}()

			err := c.Call("some.method", nil, nil)
			if err == nil {
				t.Fatal("expected error")
			}
			var rpcErr *rpcError
			if !asRPCError(err, &rpcErr) {
				t.Fatalf("error type = %T, want *rpcError", err)
			}
			if string(rpcErr.Code) != tc.wantCode || rpcErr.Message != "boom" {
				t.Fatalf("rpc error = code=%s message=%s", rpcErr.Code, rpcErr.Message)
			}
		})
	}
}

func TestCallAlwaysSendsParams(t *testing.T) {
	c, server := newPipeClient(t)
	sr := bufio.NewReader(server)

	// Read the raw line so we can assert the params field is present even when
	// the caller passes nil params.
	got := make(chan bool, 1)
	go func() {
		line, err := sr.ReadBytes('\n')
		if err != nil {
			got <- false
			return
		}
		var raw map[string]json.RawMessage
		if json.Unmarshal(line, &raw) != nil {
			got <- false
			return
		}
		_, ok := raw["params"]
		writeLine(t, server, `{"id":"1","result":{}}`)
		got <- ok
	}()

	if err := c.Call("some.method", nil, nil); err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !<-got {
		t.Error("request omitted params field")
	}
}

func TestFocusMethodsSendCorrectParams(t *testing.T) {
	cases := []struct {
		name       string
		call       func(c *Client) error
		wantMethod string
		wantKey    string
		wantVal    string
	}{
		{"pane", func(c *Client) error { return c.FocusPane("w1:p2") }, "pane.focus", "pane_id", "w1:p2"},
		{"tab", func(c *Client) error { return c.FocusTab("w1:t2") }, "tab.focus", "tab_id", "w1:t2"},
		{"workspace", func(c *Client) error { return c.FocusWorkspace("w2") }, "workspace.focus", "workspace_id", "w2"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, server := newPipeClient(t)
			sr := bufio.NewReader(server)

			done := make(chan request, 1)
			go func() {
				req := readRequest(t, sr)
				writeLine(t, server, `{"id":"`+req.ID+`","result":{}}`)
				done <- req
			}()

			if err := tc.call(c); err != nil {
				t.Fatalf("call: %v", err)
			}
			req := <-done
			if req.Method != tc.wantMethod {
				t.Errorf("method = %q, want %q", req.Method, tc.wantMethod)
			}
			params, ok := req.Params.(map[string]interface{})
			if !ok {
				t.Fatalf("params type = %T", req.Params)
			}
			if params[tc.wantKey] != tc.wantVal {
				t.Errorf("params[%q] = %v, want %q", tc.wantKey, params[tc.wantKey], tc.wantVal)
			}
		})
	}
}

func TestSubscribeThenNextEvent(t *testing.T) {
	c, server := newPipeClient(t)
	sr := bufio.NewReader(server)

	go func() {
		req := readRequest(t, sr)
		if req.Method != "events.subscribe" {
			t.Errorf("method = %q, want events.subscribe", req.Method)
		}
		// Ack, then push a pane trio (pane_focused lacks tab_id).
		writeLine(t, server, `{"id":"`+req.ID+`","result":{"type":"subscription_started"}}`)
		writeLine(t, server, `{"event":"workspace_focused","data":{"type":"workspace_focused","workspace_id":"w1"}}`)
		writeLine(t, server, `{"event":"tab_focused","data":{"type":"tab_focused","tab_id":"w1:t1","workspace_id":"w1"}}`)
		writeLine(t, server, `{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"w1:p2","workspace_id":"w1"}}`)
	}()

	if err := c.Subscribe([]Subscription{{Type: "pane.focused"}}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}

	e1, err := c.NextEvent()
	if err != nil {
		t.Fatal(err)
	}
	if e1.Event != "workspace_focused" || e1.Data.WorkspaceID != "w1" {
		t.Fatalf("event1 = %+v", e1)
	}

	e2, err := c.NextEvent()
	if err != nil {
		t.Fatal(err)
	}
	if e2.Event != "tab_focused" || e2.Data.TabID != "w1:t1" {
		t.Fatalf("event2 = %+v", e2)
	}

	e3, err := c.NextEvent()
	if err != nil {
		t.Fatal(err)
	}
	if e3.Event != "pane_focused" || e3.Data.PaneID != "w1:p2" {
		t.Fatalf("event3 = %+v", e3)
	}
	if e3.Data.TabID != "" {
		t.Errorf("pane_focused unexpectedly carried tab_id %q", e3.Data.TabID)
	}
}

func TestNextEventSkipsNonEventLines(t *testing.T) {
	c, server := newPipeClient(t)

	go func() {
		writeLine(t, server, `{"result":{"type":"subscription_started"}}`) // no "event" field
		writeLine(t, server, `not json at all`)
		writeLine(t, server, `{"event":"pane_focused","data":{"pane_id":"w1:p1"}}`)
	}()

	ev, err := c.NextEvent()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != "pane_focused" || ev.Data.PaneID != "w1:p1" {
		t.Fatalf("event = %+v", ev)
	}
}

func TestSetReadDeadline(t *testing.T) {
	c, _ := newPipeClient(t)
	if err := c.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
}

// TestDialEnvOverRealSocket exercises the full env-based Dial path against a
// real unix domain socket, complementing the in-memory net.Pipe tests.
func TestDialEnvOverRealSocket(t *testing.T) {
	// Keep the socket path short (macOS sun_path limit ~104 bytes).
	dir, err := os.MkdirTemp("/tmp", "lf-cli-")
	if err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sockPath := filepath.Join(dir, "herdr.sock")

	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { ln.Close() })

	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		r := bufio.NewReader(conn)
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req request
		if json.Unmarshal(line, &req) != nil {
			return
		}
		resp := `{"id":"` + req.ID + `","result":{"snapshot":{"focused_pane_id":"w1:p1"}}}`
		conn.Write([]byte(resp + "\n"))
	}()

	t.Setenv(EnvSocketPath, sockPath)

	c, err := DialEnv()
	if err != nil {
		t.Fatalf("DialEnv: %v", err)
	}
	defer c.Close()

	snap, err := c.Snapshot()
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snap.FocusedPaneID != "w1:p1" {
		t.Fatalf("snapshot = %+v", snap)
	}
}

// asRPCError reports whether err is a *rpcError, storing it in target.
func asRPCError(err error, target **rpcError) bool {
	e, ok := err.(*rpcError)
	if ok {
		*target = e
	}
	return ok
}
