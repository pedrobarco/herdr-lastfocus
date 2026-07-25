# herdr-lastfocus — implementation brief

Build a herdr plugin named **lastfocus** in this repo. It provides tmux-style
"last active" toggles for panes, tabs, and workspaces, backed by a background
daemon that listens to herdr focus events so history stays correct no matter how
focus changes (keybind, vim navigation, mouse, sidebar, agent focus).

Target: herdr >= 0.7.5 on macOS/Linux. Language: **Go, standard library only**
(no third-party modules). Module path: `github.com/pedrobarco/herdr-lastfocus`.

## Repository layout

```
herdr-lastfocus/
  herdr-plugin.toml
  go.mod
  main.go                     # arg dispatch: daemon | last-pane | last-tab | last-workspace
  internal/herdr/client.go    # unix-socket JSON-RPC client
  internal/history/store.go   # per-scope current/previous store
  internal/daemon/daemon.go   # event subscribe loop + lifecycle
  README.md
  LICENSE                     # MIT, author Pedro Barco
```

## herdr-plugin.toml

```toml
id = "lastfocus"
name = "Last Focus"
version = "0.1.0"
min_herdr_version = "0.7.0"
description = "tmux-style last-active pane/tab/workspace toggles via focus-event history"
platforms = ["macos", "linux"]

[[build]]
command = ["go", "build", "-o", "herdr-lastfocus", "."]

[[startup]]
command = ["./herdr-lastfocus", "daemon", "--ensure"]

[[actions]]
id = "last-pane"
title = "Focus last pane"
command = ["./herdr-lastfocus", "last-pane"]

[[actions]]
id = "last-tab"
title = "Focus last tab"
command = ["./herdr-lastfocus", "last-tab"]

[[actions]]
id = "last-workspace"
title = "Focus last workspace"
command = ["./herdr-lastfocus", "last-workspace"]
```

## herdr socket API facts (verified live against herdr 0.7.5)

- Socket path is provided via env `HERDR_SOCKET_PATH` (a unix domain socket).
  Protocol is newline-delimited JSON: send one request object per line, read one
  response line. Requests look like `{"id":"1","method":"<m>","params":{...}}`.
  Success responses are `{"id":"1","result":{...}}`, errors `{"id":..,"error":{code,message}}`.
- `session.snapshot` returns `.result.snapshot` with, among others:
  `focused_workspace_id`, `focused_tab_id`, `focused_pane_id`, and arrays
  `workspaces[]`, `tabs[]` (each `{tab_id, workspace_id, ...}`), `panes[]`
  (each `{pane_id, tab_id, workspace_id, focused, ...}`).
- `events.subscribe` params: `{"subscriptions":[{"type":"pane.focused"},{"type":"tab.focused"},{"type":"workspace.focused"}]}`.
  First response is `{"result":{"type":"subscription_started"}}`, then the socket
  stays open and pushes event lines:
    - `{"event":"pane_focused","data":{"type":"pane_focused","pane_id":"w1:p1","workspace_id":"w1"}}`
    - `{"event":"tab_focused","data":{"type":"tab_focused","tab_id":"w1:t1","workspace_id":"w1"}}`
    - `{"event":"workspace_focused","data":{"type":"workspace_focused","workspace_id":"w1"}}`
  IMPORTANT: focusing a pane emits all three events (a trio) even when only the
  pane changed within the same tab/workspace. Also note `pane_focused` does NOT
  carry a `tab_id` — only `pane_id` and `workspace_id`.
- Focus by id (all exist; pane focus by id is socket-only, the CLI cannot do it):
    - `{"method":"pane.focus","params":{"pane_id":"w1:p1"}}`
    - `{"method":"tab.focus","params":{"tab_id":"w1:t1"}}`
    - `{"method":"workspace.focus","params":{"workspace_id":"w1"}}`

## Plugin runtime env (injected by herdr)

`HERDR_SOCKET_PATH`, `HERDR_BIN_PATH`, `HERDR_PLUGIN_ID`, `HERDR_PLUGIN_ROOT`,
`HERDR_PLUGIN_CONFIG_DIR`, `HERDR_PLUGIN_STATE_DIR`, and when available
`HERDR_WORKSPACE_ID`, `HERDR_TAB_ID`, `HERDR_PANE_ID`. Store all runtime state in
`HERDR_PLUGIN_STATE_DIR`. Startup hooks additionally get `HERDR_PLUGIN_EVENT=startup`
and re-run on live server handoff (not on client attach / config reload).

## Daemon (`herdr-lastfocus daemon --ensure`)

- Idempotent launch: use a PID file + lock in `HERDR_PLUGIN_STATE_DIR`
  (e.g. `daemon.pid`, `daemon.lock`). If a live daemon already holds the lock,
  exit 0. Otherwise detach from the parent (setsid / double-fork so it survives
  the startup hook process exiting) and run the loop. Clean up stale PID files.
- On connect: send `session.snapshot`, seed each scope's `current` id from
  `focused_workspace_id` / `focused_tab_id` / `focused_pane_id`. Track the
  "current focused tab id" continuously (needed to key pane history per tab,
  since pane events lack tab_id — use the tab_focused event from the same trio,
  falling back to a snapshot lookup by pane_id if needed).
- Subscribe to the three focused events and read forever.
- Update rule with trio dedupe: for each scope, only shift current->previous and
  set the new current when THAT scope's id actually changed vs the stored current.
  This keeps same-tab pane switches from polluting tab/workspace history.
- Persist per scope as JSON `{ "current": "<id>", "previous": "<id>" }`:
    - workspace scope  -> `workspace.json`
    - tab scope        -> `tab.<workspace_id>.json`
    - pane scope       -> `pane.<tab_id>.json`
  Use atomic writes (write temp + rename). Keep it simple; a current/previous
  pair per scope is enough for an A<->B toggle (optionally keep a capped ring of
  ~200 for future use).
- Resilience: reconnect with backoff if the stream closes; exit if the socket is
  gone (server stopped). Log to a file in the state dir for debugging.

## Toggle actions (read-only; the daemon owns recording)

- `last-pane`: determine current focused tab id (env `HERDR_TAB_ID` if present,
  else `session.snapshot`), read `pane.<tab_id>.json`, and if `previous` exists
  and differs from the live focused pane, `pane.focus` it. No-op otherwise.
- `last-tab`: current workspace id (env `HERDR_WORKSPACE_ID` else snapshot), read
  `tab.<workspace_id>.json`, `tab.focus` the previous. No-op if none.
- `last-workspace`: read `workspace.json`, `workspace.focus` the previous. No-op
  if none.

## Verification

1. `go build ./...` and `go vet ./...` are clean.
2. `herdr plugin link "$PWD"` (runs the build, starts the daemon via startup hook).
   Confirm `daemon.pid` exists and the process is alive.
3. Trigger focus changes (switch tabs/panes via keys AND mouse). Confirm the
   JSON state files update, with same-tab pane switches NOT changing the tab or
   workspace files.
4. Invoke actions directly to test:
   `./herdr-lastfocus last-tab` etc. should focus the previous target.
5. `herdr config check` stays ok.

## Keybinding integration (done separately in the user's dotfiles, FYI only)

The user's `~/.config/herdr/config.toml` will bind:
  - `prefix+semicolon` -> `lastfocus.last-pane`
  - `prefix+a`         -> `lastfocus.last-tab`
  - `prefix+shift+l`   -> `lastfocus.last-workspace`
Do not edit the user's dotfiles from this repo.

## Deliverables

Implement all files above, ensure `go build` succeeds, add a clear README
(what it does, install via `herdr plugin install`/`link`, the three keybinds,
how the daemon works and its limitations), and an MIT LICENSE. Make an initial
git commit when done. Ask before adding any third-party dependency (goal is
stdlib-only).
