package svc

import (
	"context"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
)

// roomSource is what the backend uses of the room model: exactly these
// methods of *rooms.Model. Tests use a real model, wrapped where they
// record what the backend hands it.
type roomSource interface {
	Room(alias string) (rooms.Room, bool)
	RoomByID(room id.RoomID) (rooms.Room, bool)
	RoomByCanonicalAlias(alias id.RoomAlias) (rooms.Room, bool)
	Snapshot() rooms.Snapshot
	DisplayName(room id.RoomID, user id.UserID) string
	ThreadReceipt(room id.RoomID, thread id.EventID) (rooms.Receipt, bool)
	Backfill(ctx context.Context, room id.RoomID, events []*event.Event, state []*event.Event) (rooms.Changes, error)
	FillGap(ctx context.Context, room id.RoomID, gapID int64, events, state []*event.Event, nextPrevBatch string, complete bool) (rooms.Changes, error)
	Apply(ctx context.Context, b session.SyncBatch) (rooms.Changes, error)
	ApplyDecrypted(ctx context.Context, d session.LateDecrypted) (rooms.Changes, error)
	Resolver() mxfmt.Resolver
	Workspaces() []rooms.Workspace
}

var _ roomSource = (*rooms.Model)(nil)

// defaultTimestampFormat is [appearance] timestamp_format's default
// (internal/config).
const defaultTimestampFormat = "3:04 PM"

// Backend is one Matrix account behind slk's UI. Its workspace ID (the
// TeamID the UI keys state by) is "M" plus the account's user alias.
type Backend struct {
	d      Deps
	teamID string
	self   id.UserID
	rooms  roomSource
	// client makes the backend's requests (history, relations); nil
	// means store-only, which tests that need no server use.
	client   *mautrix.Client
	tsFormat string

	mu sync.Mutex
	// back is each room's back-pagination position, in memory only: the
	// store has no column for it (see backToken).
	back map[id.RoomID]backToken
	// paging serializes server pagination per room, so two fetches of
	// one room do not page the same token twice.
	paging map[id.RoomID]*sync.Mutex
	// synced is when Fetch last loaded each room (by alias), for
	// SyncedAt.
	synced map[string]time.Time
	// threadSweep is when ListFetch last asked the server for thread
	// roots; threadCounts the reply counts it reported, by root, and
	// threadJoined the roots of threads it said the account is in.
	threadSweep  time.Time
	threadCounts map[id.EventID]int
	threadJoined map[id.EventID]bool
	// localRead is how far each room (by alias) was read in this
	// process without telling the server: read-only mode's MarkRead.
	localRead map[string]string

	// ws is the account's workspaces (workspaces.go).
	ws workspaces

	// live is the live side's state (live.go); write, files and
	// profile are the other work packages' state, each defined and
	// built in that package's file.
	live    *liveState
	write   *writeState
	files   *filesState
	profile *profileState
	// verifyOut orders verification messages to the UI (verify.go).
	verifyOut verifyOut
}

// New returns the backend for an account.
func New(d Deps) *Backend {
	cli := d.Session.Client()
	return newBackend(d, d.Rooms, cli, cli.UserID)
}

// newBackend is New with the room source, client and account given, for
// tests; a nil client makes a store-only backend.
func newBackend(d Deps, rs roomSource, cli *mautrix.Client, self id.UserID) *Backend {
	b := &Backend{
		d:            d,
		self:         self,
		rooms:        rs,
		client:       cli,
		tsFormat:     d.TimestampFormat,
		back:         map[id.RoomID]backToken{},
		paging:       map[id.RoomID]*sync.Mutex{},
		synced:       map[string]time.Time{},
		threadCounts: map[id.EventID]int{},
		threadJoined: map[id.EventID]bool{},
		localRead:    map[string]string{},
		live:         newLiveState(),
		write:        newWriteState(),
		files:        newFilesState(),
		profile:      newProfileState(),
	}
	b.ws.sections = map[string]*sectionsProvider{}
	if b.tsFormat == "" {
		b.tsFormat = defaultTimestampFormat
	}
	b.teamID = "M" + rs.Resolver().UserAlias(b.self)
	return b
}

// pageLock returns room's pagination lock.
func (b *Backend) pageLock(room id.RoomID) *sync.Mutex {
	b.mu.Lock()
	defer b.mu.Unlock()
	l, ok := b.paging[room]
	if !ok {
		l = &sync.Mutex{}
		b.paging[room] = l
	}
	return l
}
