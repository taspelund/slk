// Package rooms is the account's rooms as slk sees them, and the one
// writer of the account database's room and timeline tables.
//
// Every event that reaches the backend, live from /sync or fetched by
// paging back, goes through a Model: it mints the event's TS and the
// aliases of everyone involved (ident), converts message bodies (mxfmt),
// stores the event (store), updates the room's row (name, kind, sections,
// counts) and receipts, and reports what changed. Readers (internal/matrix/svc)
// only read the store and the Model's snapshots.
//
// Room to sidebar mapping:
//
//   - a room in m.direct with one other member is a "dm", with more a
//     "group_dm"; otherwise join rule "public" is a "channel" and anything
//     else "private"
//   - names: m.room.name, else the canonical alias, else names computed
//     from the heroes (the spec's room-name algorithm), for DMs the other
//     member's display name
//   - spaces are sidebar sections: a room is in the section of its first
//     parent space (by m.space.child "order", then by name); joined rooms
//     in no space go in "Rooms", DMs in "Direct Messages". Spaces
//     themselves are not sidebar rows
//   - invites and rooms that were left are not sidebar rows yet (WP-UI)
//
// Read state, per room alias:
//
//   - LastReadTS is the TS of the account's own read receipt (main
//     timeline; threaded receipts are kept per thread)
//   - HasUnread: a message event (not a notice, not the account's own,
//     not a relation such as an edit or reaction) after the receipt
//   - MentionCount: the room's highlight_count from /sync
//   - per-thread unread state is computed from threaded receipts, because
//     the homeserver does not send per-thread counts (MSC3773 is off)
package rooms

import (
	"context"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// Model holds the account's rooms in memory, backed by the store. It is
// safe for concurrent use: Apply and Backfill are serialized; snapshots and
// lookups may run alongside them.
type Model struct{}

// New loads the account's rooms from st. self is the account's user ID.
func New(ctx context.Context, st *store.Store, ids *ident.Mapper, self id.UserID) (*Model, error) {
	panic("rooms: New not implemented (WP-Rooms)")
}

// Apply takes in one /sync response: room state, timeline events, receipts,
// unread counts, account data (m.direct, m.marked_unread) and membership
// changes. A limited timeline records a gap (store.AddGap) before the first
// new event. Events that are still encrypted are stored as such and
// replaced when a LateDecrypted delivers them.
func (m *Model) Apply(ctx context.Context, b session.SyncBatch) (Changes, error) {
	panic("rooms: Apply not implemented (WP-Rooms)")
}

// ApplyDecrypted replaces stored encrypted events with their decrypted form.
func (m *Model) ApplyDecrypted(ctx context.Context, d session.LateDecrypted) (Changes, error) {
	panic("rooms: ApplyDecrypted not implemented (WP-Rooms)")
}

// Backfill takes in events fetched by paging back in room (newest first, as
// /messages returns them with dir=b), minting TS values before the room's
// oldest. state is the response's state (member events for the senders).
func (m *Model) Backfill(ctx context.Context, room id.RoomID, events []*event.Event, state []*event.Event) (Changes, error) {
	panic("rooms: Backfill not implemented (WP-Rooms)")
}

// Changes is what one Apply, ApplyDecrypted or Backfill changed. svc turns
// it into ui messages (WP-Live).
type Changes struct {
	// Timeline lists the stored events that changed, in the order they
	// were applied.
	Timeline []TimelineChange
	// Rooms lists rooms whose sidebar row (name, kind, section, topic,
	// membership) changed; Joined and Left are rooms the account joined
	// or left in this batch.
	Rooms        []id.RoomID
	Joined, Left []id.RoomID
	// ReadStates lists rooms whose read state changed. OwnReceipts lists
	// the account's own receipts that moved (from another device or this
	// one).
	ReadStates  []id.RoomID
	OwnReceipts []Receipt
	// Typing maps a room to the users typing there now (complete list).
	Typing map[id.RoomID][]id.UserID
	// Names lists users whose display name changed.
	Names []id.UserID
}

// ChangeKind says what happened to an event.
type ChangeKind int

const (
	// Added is a message (or other displayed event) that was not stored
	// before.
	Added ChangeKind = iota + 1
	// Edited is a message whose content changed (an m.replace arrived, or
	// it was decrypted late). Event is the edited message, not the edit.
	Edited
	// Redacted is a message that was redacted.
	Redacted
	// ReactionAdded and ReactionRemoved are an m.annotation added to or
	// redacted from Event (the reacted-to message); Key is the reaction
	// and Sender who reacted.
	ReactionAdded
	ReactionRemoved
)

// TimelineChange is one change to a stored event.
type TimelineChange struct {
	Kind   ChangeKind
	Room   id.RoomID
	Event  id.EventID
	TS     string
	Thread id.EventID // thread root, "" for the main timeline
	Key    string     // ReactionAdded/Removed: the reaction key
	Sender id.UserID  // ReactionAdded/Removed: who reacted
	// Live is true for events from /sync, false for backfill.
	Live bool
}

// Receipt is the account's own read receipt in a room or thread.
type Receipt struct {
	Room   id.RoomID
	Thread id.EventID // "" for the main timeline
	Event  id.EventID
	TS     string
}

// Room is one room as the backend needs it.
type Room struct {
	ID        id.RoomID
	Alias     string // ident alias, "C…"
	Name      string
	Kind      store.RoomKind
	Encrypted bool
	DMUser    id.UserID
	Section   string
}

// Snapshot is the sidebar's view of the account. Every call returns fresh
// maps and slices that the caller may hand to the UI.
type Snapshot struct {
	Channels     []sidebar.ChannelItem
	Finder       []core.ChannelFinderItem
	ChannelNames map[string]string // room alias -> name
	UserNames    map[string]string // user alias -> display name
	ReadStates   map[string]core.ReadState
	// Sections are the sidebar sections in order, for a
	// sidebar.SectionsProvider.
	Sections []sidebar.SectionMeta
}

// Snapshot returns the current sidebar view.
func (m *Model) Snapshot() Snapshot {
	panic("rooms: Snapshot not implemented (WP-Rooms)")
}

// Room returns the room an alias names.
func (m *Model) Room(alias string) (Room, bool) {
	panic("rooms: Room not implemented (WP-Rooms)")
}

// RoomByID returns a room by its Matrix ID.
func (m *Model) RoomByID(room id.RoomID) (Room, bool) {
	panic("rooms: RoomByID not implemented (WP-Rooms)")
}

// DisplayName returns a user's display name in room ("" room for the
// global one), falling back to the localpart.
func (m *Model) DisplayName(room id.RoomID, user id.UserID) string {
	panic("rooms: DisplayName not implemented (WP-Rooms)")
}

// Resolver returns an mxfmt.Resolver over the model's aliases and names.
// Its methods do not block on I/O beyond minting a new alias.
func (m *Model) Resolver() mxfmt.Resolver {
	panic("rooms: Resolver not implemented (WP-Rooms)")
}

// ThreadReceipt returns the account's receipt in a thread.
func (m *Model) ThreadReceipt(room id.RoomID, thread id.EventID) (Receipt, bool) {
	panic("rooms: ThreadReceipt not implemented (WP-Rooms)")
}
