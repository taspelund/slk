// Package matrix is slk's Matrix backend: it drives the same TUI as the
// Slack backend from a Matrix account, talking to the homeserver directly
// (no bridge). cmd/slk wires it behind --matrix the way internal/demo is
// wired behind --demo: core ports, ui.WorkspaceReadyMsg per account, then
// live ui.*Msg values through tea.Program.Send.
//
// The UI's contract is Slack-shaped, and this backend translates into it
// rather than changing the UI:
//
//   - Message IDs are Slack-style timestamps ("%010d.%06d") that compare as
//     strings in timeline order. ident mints and persists them.
//   - User and room IDs in mentions must match [A-Z0-9]+. ident mints
//     stable aliases (U… for users, C… for rooms; never B…, which the UI
//     treats as a bot).
//   - Message bodies are Slack mrkdwn with wire mentions. mxfmt converts
//     Matrix HTML to and from that form.
//
// Subpackages:
//
//	store    one SQLite file per account: mautrix's crypto and state
//	         tables plus this backend's own tables
//	ident    TS <-> event ID and alias <-> Matrix ID mappings
//	mxfmt    Matrix HTML <-> slk mrkdwn
//	session  login, credentials, end-to-end encryption, the /sync loop
//
// The implementation plan, with the work packages that fill these in, is
// docs/superpowers/plans/2026-11-18-matrix-backend.md.
package matrix
