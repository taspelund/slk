# Matrix read receipts: who has read a message

On Matrix (`slk --matrix`), show which other users have read a message, as
Element and iamb do. Slack has no such data; nothing changes there.

## What the user sees

- **Read markers.** Under the newest message each other user has read, one
  dim row: `Seen by Alice, Bob`, collapsing past the width to
  `Seen by Alice, Bob +12`. Never the account itself or the message's
  sender. The channel pane uses the channel's timeline, the thread pane the
  thread's (markers under replies; the parent is a channel message).
- **`:seen`** on the selected message of the focused pane lists everyone
  whose position is at or after it, newest read first, with when they read
  it when the server said. The dialog is the reactions dialog under the
  title "Read receipts". Slack refuses it with a toast and leaves it out of
  the help overlay.
- **`[general] read_receipts`** (default `true`) hides the markers when
  false; `:seen` still works.

## Semantics (threaded receipts, MSC3771)

| `thread_id` | reads |
|---|---|
| `""` (unthreaded) | everything up to its event: main timeline and every thread |
| `"main"` | the main timeline only |
| a root event ID | that thread only |

A user's position in a timeline is the latest of the receipts that read it,
by slk TS. Their marker sits under the newest shown message at or before
the position: a receipt on a reaction, an edit, a state event or a hidden
(redacted) message counts there. A receipt stored before its event was
mapped (TS `""`) is mapped at read time through the ident mapper. Receipts
only move forward: `store.AdvanceReceipt` drops one that would move a
user's receipt back (federation and several devices can deliver them out of
order); the old upsert overwrote unconditionally.

## Data flow

- **Store.** `slkmx_receipts.read_at` (schema v4, `03-receipt-times.sql`) keeps
  the receipt's own `ts`; an index on `(room_id, user_id)` serves the live
  path.
- **Rooms.** `applyReceipts` stores others' receipts with `AdvanceReceipt`
  and reports the ones that moved in `Changes.Receipts` (next to
  `OwnReceipts`).
- **Pages.** Fetch, FetchOlder, FetchAround, ReadCache, thread Fetch and
  CacheRead set `MessageItem.SeenBy` (`svc.withMarkers`), bounded by the
  next shown message after the page so an older page does not claim a
  newer marker.
- **Live.** For rooms on screen (`Backend.inActive`), each user whose
  receipt moved gets one `ui.ReadMarkersMovedMsg` move per timeline the
  receipt reads (`markerAt`: a few store reads per mover). An unthreaded
  receipt also goes to the thread the UI last opened in that room; other
  threads read their markers afresh when opened.
- **UI.** `contentPane.MoveSeen` takes the user off the message they were
  under and puts them under the new one, re-rendering only those entries
  (channel pane `staleEntries`; thread pane its in-place invalidation),
  with `HoldBottom` so a pane at the bottom stays there.
- **`:seen`.** `core.ReadReceiptService.SeenBy`, store reads only, in a
  `tea.Cmd`.
