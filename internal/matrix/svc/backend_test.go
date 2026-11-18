package svc

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui/sidebar"
)

const (
	self    id.UserID = "@me:example.org"
	alice   id.UserID = "@alice:example.org"
	bob     id.UserID = "@bob:example.org"
	general id.RoomID = "!general:example.org"
)

// fakeRooms is the room model over a real store: Backfill stores events
// with TS values a real ident.Mapper mints, as rooms.Model does, and the
// rest answers from fields the test sets.
type fakeRooms struct {
	t     *testing.T
	st    *store.Store
	ids   *ident.Mapper
	names map[id.UserID]string

	mu             sync.Mutex
	readStates     map[string]core.ReadState
	threadReceipts map[id.EventID]rooms.Receipt
	backfilled     [][]id.EventID // each Backfill's event IDs, in the order given
}

func (f *fakeRooms) room(room id.RoomID) rooms.Room {
	return rooms.Room{ID: room, Alias: f.Resolver().RoomAlias(room), Name: strings.TrimPrefix(strings.Split(string(room), ":")[0], "!"), Kind: store.RoomChannel}
}

func (f *fakeRooms) Room(alias string) (rooms.Room, bool) {
	room, ok := f.Resolver().RoomID(alias)
	if !ok {
		return rooms.Room{}, false
	}
	return f.room(room), true
}

func (f *fakeRooms) RoomByID(room id.RoomID) (rooms.Room, bool) { return f.room(room), true }

func (f *fakeRooms) Snapshot() rooms.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	rs := map[string]core.ReadState{}
	for k, v := range f.readStates {
		rs[k] = v
	}
	r := f.room(general)
	return rooms.Snapshot{
		Channels:   []sidebar.ChannelItem{{ID: r.Alias, Name: r.Name, Type: "channel"}},
		ReadStates: rs,
	}
}

func (f *fakeRooms) DisplayName(_ id.RoomID, user id.UserID) string {
	if n, ok := f.names[user]; ok {
		return n
	}
	return user.Localpart()
}

func (f *fakeRooms) ThreadReceipt(_ id.RoomID, thread id.EventID) (rooms.Receipt, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.threadReceipts[thread]
	return r, ok
}

func (f *fakeRooms) Resolver() mxfmt.Resolver { return fakeResolver{f} }

// Backfill stores events, newest first, before the room's oldest.
func (f *fakeRooms) Backfill(ctx context.Context, room id.RoomID, evs []*event.Event, _ []*event.Event) (rooms.Changes, error) {
	var got []id.EventID
	for _, e := range evs {
		got = append(got, e.ID)
		ts, err := f.ids.TSForBackfill(ctx, room, e.ID, time.UnixMilli(e.Timestamp))
		if err != nil {
			return rooms.Changes{}, err
		}
		f.put(room, e, ts)
	}
	f.mu.Lock()
	f.backfilled = append(f.backfilled, got)
	f.mu.Unlock()
	return rooms.Changes{}, nil
}

// put stores e with TS ts, deriving the relation columns from its content
// as rooms.Model does.
func (f *fakeRooms) put(room id.RoomID, e *event.Event, ts string) {
	f.t.Helper()
	raw := e.Content.VeryRaw
	var c struct {
		Body      string `json:"body"`
		RelatesTo struct {
			RelType string     `json:"rel_type"`
			EventID id.EventID `json:"event_id"`
		} `json:"m.relates_to"`
	}
	_ = json.Unmarshal(raw, &c)
	row := store.Event{
		RoomID: room, ID: e.ID, TS: ts, Sender: e.Sender, Type: e.Type.Type,
		RelatesTo: c.RelatesTo.EventID, RelType: c.RelatesTo.RelType,
		Content: raw, Body: c.Body, OriginServerTS: e.Timestamp,
	}
	if row.RelType == string(event.RelThread) {
		row.ThreadRoot = row.RelatesTo
	}
	if err := f.st.PutEvent(context.Background(), row); err != nil {
		f.t.Fatalf("PutEvent: %v", err)
	}
}

// fakeResolver resolves through the fake's ident.Mapper and names.
type fakeResolver struct{ f *fakeRooms }

func (r fakeResolver) UserAlias(user id.UserID) string {
	a, err := r.f.ids.UserAlias(context.Background(), user)
	if err != nil {
		r.f.t.Errorf("UserAlias: %v", err)
	}
	return a
}

func (r fakeResolver) RoomAlias(room id.RoomID) string {
	a, err := r.f.ids.RoomAlias(context.Background(), room)
	if err != nil {
		r.f.t.Errorf("RoomAlias: %v", err)
	}
	return a
}

func (r fakeResolver) MXID(alias string) (id.UserID, bool) {
	u, ok, _ := r.f.ids.MXID(context.Background(), alias)
	return u, ok
}

func (r fakeResolver) RoomID(alias string) (id.RoomID, bool) {
	room, ok, _ := r.f.ids.RoomID(context.Background(), alias)
	return room, ok
}

func (r fakeResolver) DisplayName(user id.UserID) string { return r.f.DisplayName("", user) }

func (r fakeResolver) RoomName(room id.RoomID) string { return r.f.room(room).Name }

// harness is a Backend over a temp store, the fake room model and, when
// the test gives a handler, a client of an httptest homeserver.
type harness struct {
	t     *testing.T
	b     *Backend
	f     *fakeRooms
	st    *store.Store
	alias string // general's alias
	now   time.Time
	n     int // events made so far

	mu       sync.Mutex
	requests []string // server requests, "METHOD path?query"
}

func newHarness(t *testing.T, handler http.HandlerFunc) *harness {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fakeRooms{
		t: t, st: st, ids: ident.New(st),
		names:          map[id.UserID]string{alice: "Alice Liddell", bob: "Bob", self: "Me"},
		readStates:     map[string]core.ReadState{},
		threadReceipts: map[id.EventID]rooms.Receipt{},
	}
	h := &harness{t: t, f: f, st: st, now: time.Date(2026, 11, 18, 15, 4, 0, 0, time.Local)}
	var cli *mautrix.Client
	if handler != nil {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.mu.Lock()
			h.requests = append(h.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
			h.mu.Unlock()
			handler(w, r)
		}))
		t.Cleanup(srv.Close)
		cli, err = mautrix.NewClient(srv.URL, self, "token")
		if err != nil {
			t.Fatalf("NewClient: %v", err)
		}
	}
	h.b = newBackend(Deps{Account: "test", Store: st, IDs: f.ids}, f, cli, self)
	h.alias = f.Resolver().RoomAlias(general)
	return h
}

// ev builds an event of sender with content, a minute after the last one.
func (h *harness) ev(sender id.UserID, typ string, content map[string]any) *event.Event {
	h.t.Helper()
	h.n++
	raw, err := json.Marshal(content)
	if err != nil {
		h.t.Fatal(err)
	}
	return &event.Event{
		ID:        id.EventID("$ev" + itoa3(h.n)),
		RoomID:    general,
		Sender:    sender,
		Type:      event.Type{Type: typ, Class: event.MessageEventType},
		Timestamp: h.now.Add(time.Duration(h.n) * time.Minute).UnixMilli(),
		Content:   event.Content{VeryRaw: raw},
	}
}

func itoa3(n int) string {
	return string([]byte{byte('0' + n/100%10), byte('0' + n/10%10), byte('0' + n%10)})
}

// live stores e as /sync would, returning its TS.
func (h *harness) live(e *event.Event) string {
	h.t.Helper()
	ts, err := h.f.ids.TSForLive(context.Background(), general, e.ID, time.UnixMilli(e.Timestamp))
	if err != nil {
		h.t.Fatalf("TSForLive: %v", err)
	}
	h.f.put(general, e, ts)
	return ts
}

// text posts an m.text message live.
func (h *harness) text(sender id.UserID, body string) (*event.Event, string) {
	e := h.ev(sender, "m.room.message", map[string]any{"msgtype": "m.text", "body": body})
	return e, h.live(e)
}

func (h *harness) reqs() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.requests...)
}

// writeJSON answers a request with v.
func writeJSON(t *testing.T, w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode: %v", err)
	}
}

// wire is e as the homeserver sends it.
func wire(e *event.Event) map[string]any {
	return map[string]any{
		"event_id": e.ID, "room_id": e.RoomID, "sender": e.Sender, "type": e.Type.Type,
		"origin_server_ts": e.Timestamp, "content": json.RawMessage(e.Content.VeryRaw),
	}
}

func wires(evs ...*event.Event) []map[string]any {
	out := []map[string]any{}
	for _, e := range evs {
		out = append(out, wire(e))
	}
	return out
}

func TestTeamIDIsMPlusTheAccountsAlias(t *testing.T) {
	h := newHarness(t, nil)
	want := "M" + h.f.Resolver().UserAlias(self)
	if h.b.TeamID() != want || len(want) != 12 {
		t.Fatalf("TeamID = %q, want %q", h.b.TeamID(), want)
	}
}
