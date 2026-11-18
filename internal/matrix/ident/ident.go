// Package ident maps Matrix identifiers onto the Slack-shaped IDs slk's UI
// expects, and persists every mapping so it is identical across restarts.
//
// Message timestamps ("TS") have the form "%010d.%06d": the seconds of the
// event's origin_server_ts, then its milliseconds times 1000 plus a
// tiebreak from 0 to 999. Within one room, string order of TS values is
// timeline order:
//
//   - a live event sorts after the room's newest TS (bumped to newest+1µs
//     when its origin_server_ts is not later);
//   - a backfilled (older) event sorts before the room's oldest TS (bumped
//     to oldest-1µs when needed);
//   - an event this client sent gets its TS when the send returns, so the
//     same event arriving through /sync maps to the same TS.
//
// Aliases match ^[UC][A-Z0-9]{10}$: "U" for users, "C" for rooms. They are
// derived from a hash of the Matrix ID, with a persisted collision check.
package ident

import (
	"context"
	"errors"
	"time"

	"maunium.net/go/mautrix/id"
)

// ErrTSTaken is returned by Persistence.PutTS when ts is already used by
// another event in the room.
var ErrTSTaken = errors.New("ident: timestamp already used in room")

// ErrAliasTaken is returned by Persistence.PutAlias when alias already
// belongs to a different Matrix ID.
var ErrAliasTaken = errors.New("ident: alias already used")

// Kind says what an alias names.
type Kind int

const (
	KindUser Kind = iota + 1
	KindRoom
)

// Persistence is the storage ident needs. internal/matrix/store implements
// it; tests use an in-memory fake. All methods are safe for concurrent use.
type Persistence interface {
	// LookupTS returns the TS mapped to ev in room.
	LookupTS(ctx context.Context, room id.RoomID, ev id.EventID) (ts string, ok bool, err error)
	// LookupEvent returns the event mapped to ts in room.
	LookupEvent(ctx context.Context, room id.RoomID, ts string) (ev id.EventID, ok bool, err error)
	// RoomBounds returns the oldest and newest TS mapped in room.
	RoomBounds(ctx context.Context, room id.RoomID) (oldest, newest string, ok bool, err error)
	// PutTS records ev <-> ts in room. It returns ErrTSTaken if ts is
	// mapped to a different event in room; mapping the same pair again is
	// not an error.
	PutTS(ctx context.Context, room id.RoomID, ev id.EventID, ts string) error

	// LookupAlias returns the alias of a Matrix ID.
	LookupAlias(ctx context.Context, kind Kind, matrixID string) (alias string, ok bool, err error)
	// LookupMatrixID returns the Matrix ID an alias names.
	LookupMatrixID(ctx context.Context, alias string) (matrixID string, kind Kind, ok bool, err error)
	// PutAlias records matrixID <-> alias. It returns ErrAliasTaken if
	// alias names a different Matrix ID; recording the same pair again is
	// not an error.
	PutAlias(ctx context.Context, kind Kind, matrixID, alias string) error
}

// Mapper mints and resolves TS values and aliases. It is safe for
// concurrent use; allocation within one room is serialized.
type Mapper struct {
	p Persistence
}

// New returns a Mapper backed by p.
func New(p Persistence) *Mapper { return &Mapper{p: p} }

// TSForLive returns the TS of ev, an event that arrived through /sync,
// minting one after the room's newest TS if ev has none yet.
func (m *Mapper) TSForLive(ctx context.Context, room id.RoomID, ev id.EventID, origin time.Time) (string, error) {
	panic("ident: TSForLive not implemented (WP-Ident)")
}

// TSForBackfill returns the TS of ev, an event fetched by paging back,
// minting one before the room's oldest TS if ev has none yet.
func (m *Mapper) TSForBackfill(ctx context.Context, room id.RoomID, ev id.EventID, origin time.Time) (string, error) {
	panic("ident: TSForBackfill not implemented (WP-Ident)")
}

// TSForSent returns the TS of ev, an event this client just sent, minting
// one as for a live event. The event's /sync echo then gets the same TS
// through TSForLive.
func (m *Mapper) TSForSent(ctx context.Context, room id.RoomID, ev id.EventID, sentAt time.Time) (string, error) {
	panic("ident: TSForSent not implemented (WP-Ident)")
}

// TS returns the TS already mapped to ev, without minting.
func (m *Mapper) TS(ctx context.Context, room id.RoomID, ev id.EventID) (string, bool, error) {
	panic("ident: TS not implemented (WP-Ident)")
}

// Event returns the event mapped to ts in room.
func (m *Mapper) Event(ctx context.Context, room id.RoomID, ts string) (id.EventID, bool, error) {
	panic("ident: Event not implemented (WP-Ident)")
}

// UserAlias returns the alias of user, minting one if needed.
func (m *Mapper) UserAlias(ctx context.Context, user id.UserID) (string, error) {
	panic("ident: UserAlias not implemented (WP-Ident)")
}

// RoomAlias returns the alias of room, minting one if needed.
func (m *Mapper) RoomAlias(ctx context.Context, room id.RoomID) (string, error) {
	panic("ident: RoomAlias not implemented (WP-Ident)")
}

// MXID returns the user an alias names.
func (m *Mapper) MXID(ctx context.Context, alias string) (id.UserID, bool, error) {
	panic("ident: MXID not implemented (WP-Ident)")
}

// RoomID returns the room an alias names.
func (m *Mapper) RoomID(ctx context.Context, alias string) (id.RoomID, bool, error) {
	panic("ident: RoomID not implemented (WP-Ident)")
}
