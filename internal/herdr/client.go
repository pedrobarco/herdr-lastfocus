// Package herdr implements a minimal newline-delimited JSON-RPC client for the
// herdr unix domain socket exposed via HERDR_SOCKET_PATH.
package herdr

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"
)

// EnvSocketPath is the environment variable that carries the socket path.
const EnvSocketPath = "HERDR_SOCKET_PATH"

// request is a single JSON-RPC request line. Params is always emitted (the
// server rejects requests with a missing params field), defaulting to an empty
// object when the caller supplies none.
type request struct {
	ID     string      `json:"id"`
	Method string      `json:"method"`
	Params interface{} `json:"params"`
}

// rpcError is the error payload of a failed response. Code is kept raw because
// the server may encode it as a string (e.g. "invalid_request") or a number.
type rpcError struct {
	Code    json.RawMessage `json:"code"`
	Message string          `json:"message"`
}

func (e *rpcError) Error() string {
	code := string(e.Code)
	if len(code) == 0 {
		return fmt.Sprintf("herdr rpc error: %s", e.Message)
	}
	return fmt.Sprintf("herdr rpc error %s: %s", code, e.Message)
}

// response is a single JSON-RPC response line for a request. The Result is kept
// raw so callers can decode into a scope-specific shape.
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *rpcError       `json:"error"`
}

// Snapshot mirrors the fields of session.snapshot's .result.snapshot that this
// plugin relies on.
type Snapshot struct {
	FocusedWorkspaceID string      `json:"focused_workspace_id"`
	FocusedTabID       string      `json:"focused_tab_id"`
	FocusedPaneID      string      `json:"focused_pane_id"`
	Workspaces         []Workspace `json:"workspaces"`
	Tabs               []Tab       `json:"tabs"`
	Panes              []Pane      `json:"panes"`
}

// Workspace is a workspace entry from a snapshot.
type Workspace struct {
	WorkspaceID string `json:"workspace_id"`
}

// Tab is a tab entry from a snapshot.
type Tab struct {
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// Pane is a pane entry from a snapshot.
type Pane struct {
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
	Focused     bool   `json:"focused"`
}

// snapshotResult is the envelope returned by session.snapshot.
type snapshotResult struct {
	Snapshot Snapshot `json:"snapshot"`
}

// Client is a short-lived connection to the herdr socket. It is safe for
// sequential use; each Call sends one request line and reads one response line.
type Client struct {
	conn net.Conn
	r    *bufio.Reader
	w    *bufio.Writer

	mu  sync.Mutex
	seq int
}

// SocketPath returns the configured socket path or an error if unset.
func SocketPath() (string, error) {
	p := os.Getenv(EnvSocketPath)
	if p == "" {
		return "", fmt.Errorf("%s is not set", EnvSocketPath)
	}
	return p, nil
}

// Dial connects to the herdr socket at the given path.
func Dial(path string) (*Client, error) {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	return &Client{
		conn: conn,
		r:    bufio.NewReader(conn),
		w:    bufio.NewWriter(conn),
	}, nil
}

// DialEnv connects using the socket path from the environment.
func DialEnv() (*Client, error) {
	path, err := SocketPath()
	if err != nil {
		return nil, err
	}
	return Dial(path)
}

// Close closes the underlying connection.
func (c *Client) Close() error {
	if c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// nextID returns a monotonically increasing request id as a string.
func (c *Client) nextID() string {
	c.seq++
	return strconv.Itoa(c.seq)
}

// writeRequest marshals and writes a single request line. A nil Params is
// replaced with an empty object so the server always sees a params field.
func (c *Client) writeRequest(req request) error {
	if req.Params == nil {
		req.Params = struct{}{}
	}
	b, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	if err := c.w.WriteByte('\n'); err != nil {
		return err
	}
	return c.w.Flush()
}

// readLine reads a single newline-terminated line from the socket.
func (c *Client) readLine() ([]byte, error) {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	return line, nil
}

// Call sends a request and decodes the response result into out (which may be
// nil to discard the result).
func (c *Client) Call(method string, params interface{}, out interface{}) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := request{ID: c.nextID(), Method: method, Params: params}
	if err := c.writeRequest(req); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out != nil && len(resp.Result) > 0 {
		if err := json.Unmarshal(resp.Result, out); err != nil {
			return fmt.Errorf("decode result: %w", err)
		}
	}
	return nil
}

// Snapshot fetches the current session snapshot.
func (c *Client) Snapshot() (*Snapshot, error) {
	var res snapshotResult
	if err := c.Call("session.snapshot", nil, &res); err != nil {
		return nil, err
	}
	return &res.Snapshot, nil
}

// SnapshotOnce dials the socket from the environment, fetches one snapshot, and
// closes the connection. The server does not accept a subscription on a
// connection that has already issued a snapshot, so callers that also subscribe
// must use a dedicated connection for snapshots.
func SnapshotOnce() (*Snapshot, error) {
	c, err := DialEnv()
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.Snapshot()
}

// FocusPane focuses a pane by id (socket-only).
func (c *Client) FocusPane(paneID string) error {
	return c.Call("pane.focus", map[string]string{"pane_id": paneID}, nil)
}

// FocusTab focuses a tab by id.
func (c *Client) FocusTab(tabID string) error {
	return c.Call("tab.focus", map[string]string{"tab_id": tabID}, nil)
}

// FocusWorkspace focuses a workspace by id.
func (c *Client) FocusWorkspace(workspaceID string) error {
	return c.Call("workspace.focus", map[string]string{"workspace_id": workspaceID}, nil)
}

// CurrentPane holds the pane id and tab id of the currently focused pane.
type CurrentPane struct {
	PaneID string
	TabID  string
}

// CurrentPaneOnce dials the socket, fetches the currently focused pane, and
// closes the connection. It is cheaper than SnapshotOnce for polling use.
func CurrentPaneOnce() (CurrentPane, error) {
	c, err := DialEnv()
	if err != nil {
		return CurrentPane{}, err
	}
	defer c.Close()
	var result struct {
		Pane struct {
			PaneID string `json:"pane_id"`
			TabID  string `json:"tab_id"`
		} `json:"pane"`
	}
	if err := c.Call("pane.current", nil, &result); err != nil {
		return CurrentPane{}, err
	}
	return CurrentPane{PaneID: result.Pane.PaneID, TabID: result.Pane.TabID}, nil
}

// FocusPaneOnce dials the socket from the environment, focuses a pane by id, and
// closes the connection. A dedicated connection is used because the server
// closes a connection after it has issued a snapshot, so callers must not reuse
// a snapshot connection for a focus call.
func FocusPaneOnce(paneID string) error {
	c, err := DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.FocusPane(paneID)
}

// FocusTabOnce dials the socket, focuses a tab by id, and closes the connection.
func FocusTabOnce(tabID string) error {
	c, err := DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.FocusTab(tabID)
}

// FocusWorkspaceOnce dials the socket, focuses a workspace by id, and closes the
// connection.
func FocusWorkspaceOnce(workspaceID string) error {
	c, err := DialEnv()
	if err != nil {
		return err
	}
	defer c.Close()
	return c.FocusWorkspace(workspaceID)
}

// Subscription describes one event subscription entry.
type Subscription struct {
	Type string `json:"type"`
}

// Event is a decoded focus event pushed on a subscribed connection.
type Event struct {
	Event string    `json:"event"`
	Data  EventData `json:"data"`
}

// EventData is the payload of a focus event. Fields are populated depending on
// the event type; pane_focused notably omits tab_id.
type EventData struct {
	Type        string `json:"type"`
	PaneID      string `json:"pane_id"`
	TabID       string `json:"tab_id"`
	WorkspaceID string `json:"workspace_id"`
}

// ErrSubscriptionEnded is returned when the event stream closes cleanly.
var ErrSubscriptionEnded = errors.New("herdr: subscription ended")

// Subscribe registers for the given subscription types. The first response line
// is the subscription_started acknowledgement, which is consumed here. After it
// returns, callers use NextEvent to read pushed events.
func (c *Client) Subscribe(subs []Subscription) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	params := map[string]interface{}{"subscriptions": subs}
	req := request{ID: c.nextID(), Method: "events.subscribe", Params: params}
	if err := c.writeRequest(req); err != nil {
		return err
	}
	line, err := c.readLine()
	if err != nil {
		return err
	}
	var resp response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("decode subscribe response: %w", err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	return nil
}

// NextEvent blocks until the next event line arrives and decodes it. Lines that
// do not carry an "event" field (e.g. stray acknowledgements) are skipped.
func (c *Client) NextEvent() (*Event, error) {
	for {
		line, err := c.readLine()
		if err != nil {
			return nil, err
		}
		var ev Event
		if err := json.Unmarshal(line, &ev); err != nil {
			continue
		}
		if ev.Event == "" {
			continue
		}
		return &ev, nil
	}
}

// SetReadDeadline sets a read deadline on the underlying connection, used to
// bound blocking reads during shutdown.
func (c *Client) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}
