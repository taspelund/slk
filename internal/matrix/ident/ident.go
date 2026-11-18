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
	"crypto/sha256"
	"encoding/base32"
	"errors"
	"fmt"
	"hash/fnv"
	"strconv"
	"sync"
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
//
// The serialization lives in the Mapper, so give each Persistence one
// Mapper (one per account). Two Mappers allocating over the same
// Persistence keep TS values unique (ErrTSTaken) but could give one event
// two TS values; reads through any number of Mappers are fine, and a new
// Mapper over the same Persistence ("restart") returns identical values.
//
// Edge cases, all handled without panicking:
//
//   - A room with no mappings yet takes the natural TS (origin's
//     milliseconds, tiebreak 0), whichever direction asked.
//   - Bumps are integer microseconds, so newest+1µs from ".999999" carries
//     into the next second and oldest-1µs from ".000000" borrows from it.
//   - An origin outside what "%010d.%06d" can hold (before 1970, or after
//     9999999999 s, in the year 2286) is clamped to the nearest end before
//     bumping, so the event still lands in order.
//   - A bump that would leave that range (a backfill below
//     0000000000.000000, a live event above 9999999999.999999) returns an
//     error instead of a malformed TS.
//   - Two goroutines asking for the same unmapped event get one TS: the
//     second finds the first's mapping once it holds the room's lock.
//   - ErrTSTaken from PutTS (another writer took the candidate) re-reads
//     the room and tries a candidate strictly past the refused one, at most
//     maxAllocAttempts times.
type Mapper struct {
	p     Persistence
	rooms [roomStripes]sync.Mutex
}

const (
	// maxTSMicros is "9999999999.999999" in microseconds, the largest TS
	// the ten-digit seconds field holds.
	maxTSMicros = 9_999_999_999*1_000_000 + 999_999
	// maxAllocAttempts bounds the ErrTSTaken and ErrAliasTaken retries.
	maxAllocAttempts = 1000
	// roomStripes is how many locks rooms share; rooms hashing to one
	// stripe allocate one at a time, which is only slower.
	roomStripes = 64
	// aliasLen is the number of base32 characters after the kind prefix.
	aliasLen = 10
)

// New returns a Mapper backed by p.
func New(p Persistence) *Mapper { return &Mapper{p: p} }

// direction says which end of a room's TS range a new event goes to.
type direction int

const (
	later   direction = iota // after the newest: live and sent events
	earlier                  // before the oldest: backfilled events
)

// TSForLive returns the TS of ev, an event that arrived through /sync,
// minting one after the room's newest TS if ev has none yet.
func (m *Mapper) TSForLive(ctx context.Context, room id.RoomID, ev id.EventID, origin time.Time) (string, error) {
	return m.allocate(ctx, room, ev, origin, later)
}

// TSForBackfill returns the TS of ev, an event fetched by paging back,
// minting one before the room's oldest TS if ev has none yet.
func (m *Mapper) TSForBackfill(ctx context.Context, room id.RoomID, ev id.EventID, origin time.Time) (string, error) {
	return m.allocate(ctx, room, ev, origin, earlier)
}

// TSForSent returns the TS of ev, an event this client just sent, minting
// one as for a live event. The event's /sync echo then gets the same TS
// through TSForLive.
func (m *Mapper) TSForSent(ctx context.Context, room id.RoomID, ev id.EventID, sentAt time.Time) (string, error) {
	return m.allocate(ctx, room, ev, sentAt, later)
}

// TS returns the TS already mapped to ev, without minting.
func (m *Mapper) TS(ctx context.Context, room id.RoomID, ev id.EventID) (string, bool, error) {
	return m.p.LookupTS(ctx, room, ev)
}

// Event returns the event mapped to ts in room.
func (m *Mapper) Event(ctx context.Context, room id.RoomID, ts string) (id.EventID, bool, error) {
	return m.p.LookupEvent(ctx, room, ts)
}

// allocate returns ev's TS, minting and persisting one at dir's end of
// room if it has none. See Mapper for the edge cases.
func (m *Mapper) allocate(ctx context.Context, room id.RoomID, ev id.EventID, origin time.Time, dir direction) (string, error) {
	if room == "" || ev == "" {
		return "", fmt.Errorf("ident: TS for event %q in room %q: empty ID", ev, room)
	}
	// Most calls are for events already mapped (an echo, a re-fetch), and
	// those need no lock.
	if ts, ok, err := m.p.LookupTS(ctx, room, ev); err != nil || ok {
		return ts, err
	}
	mu := m.roomLock(room)
	mu.Lock()
	defer mu.Unlock()

	// Clamped in milliseconds first, so the multiplication cannot overflow.
	natural := min(max(origin.UnixMilli(), 0), maxTSMicros/1000) * 1000
	refused, haveRefused := int64(0), false
	for range maxAllocAttempts {
		// Re-checked under the lock: another goroutine may have mapped ev
		// while this one waited.
		ts, ok, err := m.p.LookupTS(ctx, room, ev)
		if err != nil || ok {
			return ts, err
		}
		oldest, newest, ok, err := m.p.RoomBounds(ctx, room)
		if err != nil {
			return "", err
		}
		c := natural
		if ok {
			c, err = bumped(c, oldest, newest, dir)
			if err != nil {
				return "", fmt.Errorf("ident: TS for %s in %s: %w", ev, room, err)
			}
		}
		if haveRefused {
			if dir == later {
				c = max(c, refused+1)
			} else {
				c = min(c, refused-1)
			}
		}
		if c < 0 || c > maxTSMicros {
			return "", fmt.Errorf("ident: TS for %s in %s: no TS left %s the room's range", ev, room, dir)
		}
		ts = formatTS(c)
		switch err := m.p.PutTS(ctx, room, ev, ts); {
		case err == nil:
			return ts, nil
		case errors.Is(err, ErrTSTaken):
			refused, haveRefused = c, true
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("ident: TS for %s in %s: %d candidates taken", ev, room, maxAllocAttempts)
}

// bumped moves c past the room's bounds in dir: to newest+1µs when it is
// not after newest, to oldest-1µs when it is not before oldest. The result
// may be out of range; allocate checks.
func bumped(c int64, oldest, newest string, dir direction) (int64, error) {
	if dir == later {
		n, err := parseTS(newest)
		if err != nil {
			return 0, err
		}
		return max(c, n+1), nil
	}
	o, err := parseTS(oldest)
	if err != nil {
		return 0, err
	}
	return min(c, o-1), nil
}

func (d direction) String() string {
	if d == later {
		return "after"
	}
	return "before"
}

// roomLock returns the stripe of m.rooms that serializes room.
func (m *Mapper) roomLock(room id.RoomID) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(room))
	return &m.rooms[h.Sum32()%roomStripes]
}

// formatTS renders microseconds since the epoch, 0 <= us <= maxTSMicros,
// as "%010d.%06d".
func formatTS(us int64) string {
	return fmt.Sprintf("%010d.%06d", us/1_000_000, us%1_000_000)
}

// parseTS is formatTS's inverse. It accepts only that exact shape, so a
// malformed value from storage is an error rather than a wrong order.
func parseTS(ts string) (int64, error) {
	if len(ts) != 17 || ts[10] != '.' {
		return 0, fmt.Errorf("ident: malformed TS %q", ts)
	}
	var us int64
	for i := 0; i < len(ts); i++ {
		if i == 10 {
			continue
		}
		c := ts[i]
		if c < '0' || c > '9' {
			return 0, fmt.Errorf("ident: malformed TS %q", ts)
		}
		us = us*10 + int64(c-'0')
	}
	return us, nil
}

// UserAlias returns the alias of user, minting one if needed.
func (m *Mapper) UserAlias(ctx context.Context, user id.UserID) (string, error) {
	return m.alias(ctx, KindUser, string(user))
}

// RoomAlias returns the alias of room, minting one if needed.
func (m *Mapper) RoomAlias(ctx context.Context, room id.RoomID) (string, error) {
	return m.alias(ctx, KindRoom, string(room))
}

// MXID returns the user an alias names.
func (m *Mapper) MXID(ctx context.Context, alias string) (id.UserID, bool, error) {
	mxid, ok, err := m.resolve(ctx, KindUser, alias)
	return id.UserID(mxid), ok, err
}

// RoomID returns the room an alias names.
func (m *Mapper) RoomID(ctx context.Context, alias string) (id.RoomID, bool, error) {
	mxid, ok, err := m.resolve(ctx, KindRoom, alias)
	return id.RoomID(mxid), ok, err
}

// alias returns matrixID's alias, persisting the first candidate (see
// aliasCandidate) that is free or already its own. Candidates are tried in
// a fixed order and PutAlias accepts a repeated pair, so concurrent calls
// for one ID agree without a lock.
func (m *Mapper) alias(ctx context.Context, kind Kind, matrixID string) (string, error) {
	if matrixID == "" {
		return "", errors.New("ident: alias for empty Matrix ID")
	}
	if a, ok, err := m.p.LookupAlias(ctx, kind, matrixID); err != nil || ok {
		return a, err
	}
	for n := range maxAllocAttempts {
		a := aliasCandidate(kind, matrixID, n)
		switch err := m.p.PutAlias(ctx, kind, matrixID, a); {
		case err == nil:
			return a, nil
		case errors.Is(err, ErrAliasTaken):
		default:
			return "", err
		}
	}
	return "", fmt.Errorf("ident: alias for %s: %d candidates taken", matrixID, maxAllocAttempts)
}

// aliasCandidate is matrixID's n-th alias candidate: the kind's prefix,
// then the first aliasLen characters of the uppercase base32 SHA-256 of
// matrixID (n == 0) or of matrixID, NUL, n in decimal (n > 0).
func aliasCandidate(kind Kind, matrixID string, n int) string {
	in := matrixID
	if n > 0 {
		in += "\x00" + strconv.Itoa(n)
	}
	sum := sha256.Sum256([]byte(in))
	return kindPrefix(kind) + base32.StdEncoding.EncodeToString(sum[:])[:aliasLen]
}

func kindPrefix(kind Kind) string {
	if kind == KindRoom {
		return "C"
	}
	return "U"
}

// resolve returns the Matrix ID alias names if it names one of kind.
func (m *Mapper) resolve(ctx context.Context, kind Kind, alias string) (string, bool, error) {
	if len(alias) != 1+aliasLen || alias[:1] != kindPrefix(kind) {
		return "", false, nil
	}
	mxid, k, ok, err := m.p.LookupMatrixID(ctx, alias)
	if err != nil || !ok || k != kind {
		return "", false, err
	}
	return mxid, true, nil
}
