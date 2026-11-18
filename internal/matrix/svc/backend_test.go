package svc

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

const (
	self    id.UserID = "@me:example.org"
	alice   id.UserID = "@alice:example.org"
	bob     id.UserID = "@bob:example.org"
	general id.RoomID = "!general:example.org"
)

// recRooms is the real room model, recording the fetched events the
// backend hands it.
type recRooms struct {
	*rooms.Model

	mu         sync.Mutex
	backfilled [][]id.EventID // each Backfill's event IDs, in the order given
	filled     []fillCall
}

// fillCall is one FillGap call.
type fillCall struct {
	gap    int64
	events []id.EventID
	next   string
}

func eventIDs(evs []*event.Event) []id.EventID {
	var out []id.EventID
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func (r *recRooms) Backfill(ctx context.Context, room id.RoomID, evs, state []*event.Event) (rooms.Changes, error) {
	r.mu.Lock()
	r.backfilled = append(r.backfilled, eventIDs(evs))
	r.mu.Unlock()
	return r.Model.Backfill(ctx, room, evs, state)
}

func (r *recRooms) FillGap(ctx context.Context, room id.RoomID, gap int64, evs, state []*event.Event, next string, complete bool) (rooms.Changes, error) {
	r.mu.Lock()
	r.filled = append(r.filled, fillCall{gap, eventIDs(evs), next})
	r.mu.Unlock()
	return r.Model.FillGap(ctx, room, gap, evs, state, next, complete)
}

func (r *recRooms) backfills() [][]id.EventID {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.backfilled)
}

func (r *recRooms) fills() []fillCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.filled)
}

// harness is a Backend over a temp store, a real room model in which
// self, alice and bob are in the public room general, and, when the test
// gives a handler, a client of an httptest homeserver.
type harness struct {
	t     *testing.T
	b     *Backend
	m     *recRooms
	st    *store.Store
	ids   *ident.Mapper
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
	ids := ident.New(st)
	model, err := rooms.New(ctx, st, ids, self)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	h := &harness{t: t, m: &recRooms{Model: model}, st: st, ids: ids, now: time.Date(2026, 11, 1, 15, 4, 0, 0, time.Local)}
	resp := lsync()
	ljoin(resp, general).State.Events = []*event.Event{
		lstate(t, "m.room.name", "", alice, map[string]any{"name": "general"}),
		lstate(t, "m.room.join_rules", "", alice, map[string]any{"join_rule": "public"}),
		lmember(t, alice, "Alice Liddell"), lmember(t, bob, "Bob"), lmember(t, self, "Me"),
	}
	h.apply(resp)
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
	h.b = newBackend(Deps{Account: "test", Store: st, IDs: ids, Rooms: model}, h.m, cli, self)
	h.alias = model.Resolver().RoomAlias(general)
	return h
}

// apply applies resp as a /sync response.
func (h *harness) apply(resp *mautrix.RespSync) {
	h.t.Helper()
	if _, err := h.m.Apply(context.Background(), session.SyncBatch{Resp: resp}); err != nil {
		h.t.Fatalf("Apply: %v", err)
	}
}

// res is the model's resolver.
func (h *harness) res() mxfmt.Resolver { return h.m.Resolver() }

// tsOf returns ev's TS.
func (h *harness) tsOf(ev id.EventID) string {
	h.t.Helper()
	ts, ok, err := h.ids.TS(context.Background(), general, ev)
	if err != nil || !ok {
		h.t.Fatalf("TS of %s: %v %v", ev, ok, err)
	}
	return ts
}

// redact redacts ev through /sync.
func (h *harness) redact(ev id.EventID) {
	h.t.Helper()
	e := h.ev(alice, "m.room.redaction", map[string]any{"redacts": ev})
	e.Redacts = ev
	h.live(e)
}

// receipt applies an own receipt on ev (in thread, "" for none).
func (h *harness) receipt(ev id.EventID, thread string) {
	h.t.Helper()
	r := map[string]any{"ts": 1}
	if thread != "" {
		r["thread_id"] = thread
	}
	resp := lsync()
	ljoin(resp, general).Ephemeral.Events = []*event.Event{lmk(h.t, "m.receipt", "", "", 0, nil, map[string]any{
		string(ev): map[string]any{"m.read": map[string]any{string(self): r}},
	})}
	h.apply(resp)
}

// ev builds an event of sender with content, a minute after the last
// one, as one parsed from the server's JSON.
func (h *harness) ev(sender id.UserID, typ string, content map[string]any) *event.Event {
	h.t.Helper()
	h.n++
	e := lmk(h.t, typ, id.EventID("$ev"+itoa3(h.n)), sender, h.now.Add(time.Duration(h.n)*time.Minute).UnixMilli(), nil, content)
	e.RoomID = general
	return e
}

func itoa3(n int) string {
	return string([]byte{byte('0' + n/100%10), byte('0' + n/10%10), byte('0' + n%10)})
}

// live applies e through /sync, returning its TS ("" for an event the
// model does not store, such as a redaction).
func (h *harness) live(e *event.Event) string {
	h.t.Helper()
	h.apply(timeline(general, e))
	ts, _, _ := h.ids.TS(context.Background(), general, e.ID)
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
	want := "M" + h.res().UserAlias(self)
	if h.b.TeamID() != want || len(want) != 12 {
		t.Fatalf("TeamID = %q, want %q", h.b.TeamID(), want)
	}
}

// TestReadOnlyChangesNothing: a read-only backend opens a room without
// marking it read and has no port that changes the account.
func TestReadOnlyChangesNothing(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/messages") {
			writeJSON(t, w, map[string]any{"start": "s0", "chunk": []any{}})
			return
		}
		http.NotFound(w, r)
	})
	h.b.d.ReadOnly = true
	h.b.d.Dir = t.TempDir()
	h.apply(timeline(general, h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hi"})))
	p := h.b.ports()
	msg, ok := p.channels.Fetch(ids.ChannelID(h.alias), "").(ui.MessagesLoadedMsg)
	if !ok || len(msg.Messages) != 1 || msg.MarkedTS != "" {
		t.Fatalf("Fetch = %#v", msg)
	}
	time.Sleep(100 * time.Millisecond) // a mark would run in a goroutine
	for _, r := range h.reqs() {
		if !strings.HasPrefix(r, "GET ") {
			t.Errorf("read-only backend sent %s", r)
		}
	}
	if p.channels.MarkRead != nil || p.messages.Send != nil || p.threads.SendReply != nil || p.reactions != nil || p.presence != nil {
		t.Errorf("read-only backend has write ports: %+v", p)
	}
	res, ok := p.files.Upload(h.alias, "", "", nil)().(ui.UploadResultMsg)
	if !ok || !errors.Is(res.Err, errReadOnly) {
		t.Errorf("Upload = %#v, want errReadOnly", res)
	}
}
