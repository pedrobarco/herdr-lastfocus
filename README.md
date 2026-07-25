# herdr-lastfocus

tmux-style "last active" toggles for [herdr](https://herdr.dev) panes, tabs, and
workspaces. Jump back and forth between the two most recent targets at each
scope — like tmux's `prefix + ;` (last pane) and `prefix + l` (last window).

Focus history is maintained by a small background daemon that subscribes to
herdr focus events, so the "last active" target stays correct no matter how
focus changed: keybind, vim-style navigation, mouse, sidebar, or agent focus.

Requires herdr `>= 0.7.5` on macOS or Linux. Written in Go using only the
standard library.

## What it does

Three actions, each toggling within its scope:

| Action              | Scope     | Behaviour                                            |
| ------------------- | --------- | ---------------------------------------------------- |
| `lastfocus.last-pane`      | current tab       | focus the previously active pane in this tab   |
| `lastfocus.last-tab`       | current workspace | focus the previously active tab in this workspace |
| `lastfocus.last-workspace` | global            | focus the previously active workspace          |

Invoking an action while sitting on the "current" target sends you to the
"previous" one; invoking it again returns you — an A↔B toggle.

## Relation to native herdr

herdr ships a native `last_pane` action (unset by default, e.g.
`last_pane = "prefix+tab"`) that toggles the last-focused pane globally. It has
no native equivalent for tabs or workspaces — only positional (`next_/previous_`)
and indexed (`switch_tab`/`switch_workspace = prefix+1..9`) movement. This plugin
adds per-scope most-recently-used toggles, so its main value is
`last-tab` and `last-workspace`; use `last-pane` if you prefer per-tab pane
scoping over herdr's global `last_pane`.

## Install

Clone the repo, then link or install it with herdr. Both run the build and start
the daemon via the startup hook:

```sh
# For local development (symlinks the plugin, rebuilds on changes):
herdr plugin link "$PWD"

# Or install a copy:
herdr plugin install "$PWD"
```

Verify:

```sh
herdr config check          # stays ok
cat "$HERDR_PLUGIN_STATE_DIR/daemon.pid"   # daemon pid, process should be alive
```

## Keybindings

Bind the actions in `~/.config/herdr/config.toml` (example — adjust to taste):

```toml
prefix+semicolon = "lastfocus.last-pane"
prefix+a         = "lastfocus.last-tab"
prefix+shift+l   = "lastfocus.last-workspace"
```

## How the daemon works

- The `startup` hook runs `herdr-lastfocus daemon --ensure`. This is idempotent:
  it uses a PID file and an advisory lock (`daemon.pid`, `daemon.lock`) in the
  plugin state dir. If a live daemon already holds the lock it exits 0; otherwise
  it clears any stale PID file and spawns a detached (`setsid`) child that
  survives the hook process exiting.
- On connect the daemon takes a `session.snapshot` (on its own connection) and
  seeds each scope's current id, then subscribes to `pane.focused`,
  `tab.focused`, and `workspace.focused` on a second connection and reads events
  forever. A snapshot and a subscription must use separate connections: the
  server closes a connection once it has issued a snapshot.
- For each event it shifts `current -> previous` and sets the new current **only
  when that scope's id actually changed**, so same-tab pane switches never
  pollute tab or workspace history.
- A focus change is delivered as a trio in the order `pane_focused` ->
  `tab_focused` -> `workspace_focused`, and `pane_focused` carries no `tab_id`.
  The pane event therefore arrives before the tab that owns it is known, so the
  daemon buffers the pending pane and records it against the tab resolved by the
  trailing `tab_focused` (falling back to the tracked current tab, or a snapshot
  lookup by `pane_id`, for a lone pane event).
- State is persisted as atomic JSON files (write temp + rename) in the state dir:
  - `workspace.json` — workspace scope
  - `tab.<workspace_id>.json` — tab scope, per workspace
  - `pane.<tab_id>.json` — pane scope, per tab

  Each file is `{ "current": "<id>", "previous": "<id>", "ring": [...] }`. The
  current/previous pair drives the toggle; a capped ring (~200) is retained for
  potential future use.
- If the event stream drops, the daemon reconnects with backoff. If the socket
  disappears (server stopped), it exits. A `daemon.log` in the state dir records
  activity for debugging.

The toggle actions themselves are read-only: they read the relevant scope file
and call `pane.focus` / `tab.focus` / `workspace.focus` on the `previous` id (a
no-op when there is no previous or it already matches the live focus). The daemon
is the sole owner of recording history.

## Limitations

- `pane_focused` events do not carry a `tab_id` and arrive before the
  `tab_focused` in their trio. The daemon defers recording a pane until that
  `tab_focused` resolves its tab (falling back to the tracked current tab, or a
  snapshot lookup by `pane_id`) to key pane history per tab. In rare races the
  fallback snapshot may be needed.
- Focusing a pane by id is socket-only; the daemon and actions talk directly to
  the herdr socket via `HERDR_SOCKET_PATH`.
- History lives in `HERDR_PLUGIN_STATE_DIR`. Current/previous survive daemon
  restarts, but removing the state files resets history.
- One daemon per herdr server; the lock prevents duplicates.

## Layout

```
herdr-plugin.toml           # plugin manifest (build, startup, actions)
main.go                     # arg dispatch: daemon | last-pane | last-tab | last-workspace
internal/herdr/client.go    # unix-socket newline-delimited JSON-RPC client
internal/history/store.go   # per-scope current/previous store with atomic writes
internal/daemon/daemon.go   # ensure/launch + event subscribe loop + lifecycle
```

## License

MIT — see [LICENSE](./LICENSE).
