# Matrix backend — implementation plan

> **Status:** WP-0 (skeleton and contracts) done; Wave A in progress.
> **Base:** gammons/slk `b03f9365` (2026-11-17), mautrix-go `main` at
> `3e30ad8f` (pure-Go encryption by default; move to v0.34.0 when released).

## Goal

Drive slk's existing TUI from a Matrix account, talking to the homeserver
directly (no bridge). Slack stays on slk's own backend. Slack and Matrix in
one process comes last (M4) and needs refactor Phase 2 steps 6–8 of
`2026-09-06-architecture-refactor.md` first.

## How it plugs in

The same way `internal/demo` does (`internal/demo/demo.go`, `cmd/slk/demo.go`):
build the App, install the `internal/core` ports with the App's setters, send a
`ui.WorkspaceReadyMsg` per account and a `ui.ConnectionStateMsg`, then push
live `ui.*Msg` values through `tea.Program.Send`. Port methods return
`core.Msg`, but the reducers only understand `internal/ui` message types, so
the backend imports `internal/ui` (the demo does too).

Entry points (hidden flags, `cmd/slk/matrix.go`): `slk --matrix-login <account>`
and `slk --matrix <account>`. Each account keeps `session.json` (0600) and
`matrix.db` in `$XDG_DATA_HOME/slk/matrix/<account>`.

## Rules

- New code lives in `internal/matrix/**` and `cmd/slk/matrix*.go`. UI changes
  are rare, small and flagged (WP-UI), so upstream merges stay cheap.
- Translate into the UI's Slack-shaped contract; don't change the UI to fit
  Matrix:
  - **Message IDs** are `"%010d.%06d"` timestamps (origin_server_ts seconds;
    milliseconds × 1000 plus a 0–999 tiebreak). Within a room their string
    order is timeline order: live events are bumped after the room's newest TS,
    backfilled events before its oldest. An event this client sends gets its TS
    from the send response, so its /sync echo maps to the same TS. The UI
    compares these as strings (e.g. `messages/model.go` "new" divider,
    `app.go` `recordChannelMark`) and parses them as times (day separators).
  - **Aliases** are `U` (users) or `C` (rooms) plus 10 base32 characters from
    a hash of the Matrix ID, collision-checked. Mentions must match
    `<@[A-Z0-9]+>` / `<#[A-Z0-9]+(|name)?>` (`messages/render.go`); user IDs
    starting with `B` are treated as bots (`app.go`).
  - **Bodies** are Slack mrkdwn with wire mentions in, and the composer's
    CommonMark plus wire mentions out (`compose.TranslateMentionsForSend`).
- Every TS and alias is persisted before it reaches the UI.
- `matrix.db` is the backend's own database (modernc SQLite, like slk's
  cache), shared with mautrix's crypto and state stores. slk's
  `internal/cache` is not used.
- End-to-end encryption is required: Element encrypts new DMs and private
  rooms by default.

## Work packages

| WP | Owns | Depends on |
|---|---|---|
| 0 Skeleton and contracts | `internal/matrix/*/` stubs, `cmd/slk/matrix.go`, `go.mod` | — |
| Store | `internal/matrix/store/` | 0 |
| Ident | `internal/matrix/ident/` | 0 (in-memory fake of `ident.Persistence`) |
| Format | `internal/matrix/mxfmt/` | 0 |
| Harness | `internal/matrix/e2e/` (tag `matrixe2e`), `scripts/matrix-dev/` | 0 |
| Session | `internal/matrix/session/` | Store (Open), Harness |
| Rooms | `internal/matrix/rooms/` | Wave A |
| Read | `internal/matrix/svc/read*.go` | Wave A, Rooms |
| Live | `internal/matrix/svc/live*.go` | Read |
| Write | `internal/matrix/svc/write*.go` | Read |
| Media | `internal/matrix/media/`, `svc/files.go` | Read |
| Profile | `internal/matrix/svc/profile.go`, `presence.go` | Read |
| UI | small flagged `internal/ui` changes | M2 |
| Verify | SAS verification modal, key-backup upload | M2 |
| Mixed | Slack and Matrix workspaces in one process | refactor Phase 2 |

Contracts for Rooms, svc and Media are written when Wave A lands.

Milestones: **M1** read-only (log in, recovery-key restore, browse rooms and
threads, page back, live new messages); **M2** daily driver (send, reply,
react, edit, delete, files, unread, notifications); **M3** UI and
verification polish; **M4** one window.

## Testing

stdlib `testing`, white-box, as `AGENTS.md` says. Anything that needs a
homeserver runs against a throwaway Synapse in Docker (WP-Harness) behind the
`matrixe2e` build tag, never against a real account.
