package rooms

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui/sidebar"
)

const (
	me    = id.UserID("@me:x")
	alice = id.UserID("@alice:x")
	bob   = id.UserID("@bob:x")
	carol = id.UserID("@carol:x")
)

// fixture is a Model over a fresh store.
type fixture struct {
	t   *testing.T
	st  *store.Store
	ids *ident.Mapper
	m   *Model
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	st, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	f := &fixture{t: t, st: st}
	f.m = f.reopen()
	return f
}

// reopen returns a new Model (and Mapper) over the fixture's store, as
// after a restart.
func (f *fixture) reopen() *Model {
	f.t.Helper()
	f.ids = ident.New(f.st)
	m, err := New(context.Background(), f.st, f.ids, me)
	if err != nil {
		f.t.Fatalf("New: %v", err)
	}
	return m
}

func (f *fixture) apply(resp *mautrix.RespSync) Changes {
	f.t.Helper()
	ch, err := f.m.Apply(context.Background(), session.SyncBatch{Resp: resp})
	if err != nil {
		f.t.Fatalf("Apply: %v", err)
	}
	return ch
}

func (f *fixture) event(rid id.RoomID, ev id.EventID) store.Event {
	f.t.Helper()
	e, ok, err := f.st.GetEvent(context.Background(), rid, ev)
	if err != nil || !ok {
		f.t.Fatalf("GetEvent %s: ok=%v err=%v", ev, ok, err)
	}
	return e
}

func (f *fixture) ts(rid id.RoomID, ev id.EventID) string {
	f.t.Helper()
	ts, ok, err := f.ids.TS(context.Background(), rid, ev)
	if err != nil || !ok {
		f.t.Fatalf("TS of %s: ok=%v err=%v", ev, ok, err)
	}
	return ts
}

func (f *fixture) roomAlias(rid id.RoomID) string {
	f.t.Helper()
	a, err := f.ids.RoomAlias(context.Background(), rid)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

func (f *fixture) userAlias(u id.UserID) string {
	f.t.Helper()
	a, err := f.ids.UserAlias(context.Background(), u)
	if err != nil {
		f.t.Fatal(err)
	}
	return a
}

// mk builds an event the way one parsed from a /sync response looks
// (content raw, Type without a class).
func mk(t *testing.T, typ string, evID id.EventID, sender id.UserID, ms int64, stateKey *string, content any) *event.Event {
	t.Helper()
	raw := map[string]any{"type": typ, "sender": sender, "origin_server_ts": ms, "content": content}
	if evID != "" {
		raw["event_id"] = evID
	}
	if stateKey != nil {
		raw["state_key"] = *stateKey
	}
	b, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var evt event.Event
	if err := json.Unmarshal(b, &evt); err != nil {
		t.Fatal(err)
	}
	return &evt
}

func state(t *testing.T, typ string, key string, sender id.UserID, content any) *event.Event {
	return mk(t, typ, id.EventID(fmt.Sprintf("$state-%s-%s", typ, key)), sender, 1, &key, content)
}

func memberEv(t *testing.T, user id.UserID, name string) *event.Event {
	return state(t, "m.room.member", string(user), user, map[string]any{"membership": "join", "displayname": name})
}

func txt(t *testing.T, evID id.EventID, sender id.UserID, ms int64, body string) *event.Event {
	return mk(t, "m.room.message", evID, sender, ms, nil, map[string]any{"msgtype": "m.text", "body": body})
}

func newSync() *mautrix.RespSync {
	return &mautrix.RespSync{Rooms: mautrix.RespSyncRooms{
		Join:   map[id.RoomID]*mautrix.SyncJoinedRoom{},
		Invite: map[id.RoomID]*mautrix.SyncInvitedRoom{},
		Leave:  map[id.RoomID]*mautrix.SyncLeftRoom{},
	}}
}

func join(resp *mautrix.RespSync, rid id.RoomID) *mautrix.SyncJoinedRoom {
	jr := resp.Rooms.Join[rid]
	if jr == nil {
		jr = &mautrix.SyncJoinedRoom{}
		resp.Rooms.Join[rid] = jr
	}
	return jr
}

func intp(n int) *int { return &n }

// world is an initial sync: two spaces, rooms in them (one in both), a
// public and a private room in no space, a DM and a group DM.
func world(t *testing.T) *mautrix.RespSync {
	resp := newSync()
	resp.AccountData.Events = append(resp.AccountData.Events, mk(t, "m.direct", "", "", 0, nil, map[string]any{
		string(alice): []string{"!dm:x"},
		string(bob):   []string{"!group:x"},
		string(carol): []string{"!group:x"},
	}))
	space := func(rid id.RoomID, name string, children map[string]string) {
		jr := join(resp, rid)
		jr.State.Events = append(jr.State.Events,
			state(t, "m.room.create", "", alice, map[string]any{"type": "m.space"}),
			state(t, "m.room.name", "", alice, map[string]any{"name": name}),
		)
		for child, order := range children {
			c := map[string]any{"via": []string{"x"}}
			if order != "" {
				c["order"] = order
			}
			jr.State.Events = append(jr.State.Events, state(t, "m.space.child", child, alice, c))
		}
	}
	space("!space1:x", "Engineering", map[string]string{"!eng:x": "b", "!both:x": "a", "!tie:x": ""})
	space("!space2:x", "Alpha", map[string]string{"!both:x": "", "!tie:x": ""})
	named := func(rid id.RoomID, name string, extra ...*event.Event) *mautrix.SyncJoinedRoom {
		jr := join(resp, rid)
		jr.State.Events = append(jr.State.Events, state(t, "m.room.name", "", alice, map[string]any{"name": name}))
		jr.State.Events = append(jr.State.Events, extra...)
		return jr
	}
	named("!eng:x", "eng")
	named("!both:x", "both")
	named("!tie:x", "tie")
	named("!general:x", "general", state(t, "m.room.join_rules", "", alice, map[string]any{"join_rule": "public"}),
		state(t, "m.room.topic", "", alice, map[string]any{"topic": "chat"}))
	named("!secret:x", "secret", state(t, "m.room.join_rules", "", alice, map[string]any{"join_rule": "invite"}),
		state(t, "m.room.encryption", "", alice, map[string]any{"algorithm": "m.megolm.v1.aes-sha2"}))
	dm := join(resp, "!dm:x")
	dm.Summary = mautrix.LazyLoadSummary{Heroes: []id.UserID{alice}, JoinedMemberCount: intp(2)}
	dm.State.Events = append(dm.State.Events, memberEv(t, alice, "Alice"), memberEv(t, me, "Me"))
	group := join(resp, "!group:x")
	group.Summary = mautrix.LazyLoadSummary{Heroes: []id.UserID{bob, carol}, JoinedMemberCount: intp(3)}
	group.State.Events = append(group.State.Events, memberEv(t, bob, "Bob"), memberEv(t, carol, "Carol"))
	return resp
}

func TestInitialSyncRoomsAndSections(t *testing.T) {
	f := newFixture(t)
	ch := f.apply(world(t))
	snap := f.m.Snapshot()

	s1, s2 := f.roomAlias("!space1:x"), f.roomAlias("!space2:x")
	wantSections := []sidebar.SectionMeta{
		{ID: s2, Name: "Alpha", Type: "standard"},
		{ID: s1, Name: "Engineering", Type: "standard"},
		{ID: "rooms", Name: "Rooms", Type: "channels"},
		{ID: "dms", Name: "Direct Messages", Type: "direct_messages"},
	}
	if !reflect.DeepEqual(snap.Sections, wantSections) {
		t.Fatalf("Sections = %+v\nwant %+v", snap.Sections, wantSections)
	}
	item := func(rid id.RoomID, name, typ, section string, order int, dm id.UserID) sidebar.ChannelItem {
		it := sidebar.ChannelItem{ID: f.roomAlias(rid), Name: name, Type: typ, Section: section, SectionOrder: order}
		if dm != "" {
			it.DMUserID = f.userAlias(dm)
		}
		return it
	}
	want := []sidebar.ChannelItem{
		// "tie" is in both spaces without an order: Alpha wins by name.
		item("!tie:x", "tie", "private", s2, 1, ""),
		// "both" has order "a" in Engineering, none in Alpha.
		item("!both:x", "both", "private", s1, 2, ""),
		item("!eng:x", "eng", "private", s1, 2, ""),
		item("!general:x", "general", "channel", "rooms", 3, ""),
		item("!secret:x", "secret", "private", "rooms", 3, ""),
		item("!dm:x", "Alice", "dm", "dms", 4, alice),
		item("!group:x", "Bob, Carol", "group_dm", "dms", 4, ""),
	}
	if !reflect.DeepEqual(snap.Channels, want) {
		t.Fatalf("Channels =\n%+v\nwant\n%+v", snap.Channels, want)
	}
	if len(snap.Finder) != len(want) || snap.Finder[5] != (core.ChannelFinderItem{
		ID: want[5].ID, Name: "Alice", Type: "dm", Joined: true, DMUserID: f.userAlias(alice),
	}) {
		t.Fatalf("Finder = %+v", snap.Finder)
	}
	if snap.ChannelNames[f.roomAlias("!general:x")] != "general" || snap.UserNames[f.userAlias(bob)] != "Bob" {
		t.Fatalf("names: %v %v", snap.ChannelNames, snap.UserNames)
	}
	for _, sp := range []id.RoomID{"!space1:x", "!space2:x"} {
		if r, ok := f.m.RoomByID(sp); !ok || r.Section != "" {
			t.Fatalf("space %s: %+v %v (spaces are not sidebar rows)", sp, r, ok)
		}
	}
	r, ok := f.m.Room(f.roomAlias("!secret:x"))
	if !ok || !r.Encrypted || r.Kind != store.RoomPrivate || r.Section != "rooms" || r.ID != "!secret:x" {
		t.Fatalf("Room(secret) = %+v %v", r, ok)
	}
	if r, _ := f.m.RoomByID("!dm:x"); r.DMUser != alice || r.Kind != store.RoomDM {
		t.Fatalf("dm = %+v", r)
	}
	if len(ch.Joined) != 9 || len(ch.Rooms) != 9 {
		t.Fatalf("Joined = %v, Rooms = %v; want all 9 rooms", ch.Joined, ch.Rooms)
	}
	row, _, _ := f.st.GetRoom(context.Background(), "!general:x")
	if row.Topic != "chat" || row.Kind != store.RoomChannel || row.Name != "general" {
		t.Fatalf("stored general = %+v", row)
	}

	// Moving the DM out of m.direct makes it a private room in Rooms.
	resp := newSync()
	resp.AccountData.Events = append(resp.AccountData.Events, mk(t, "m.direct", "", "", 0, nil, map[string]any{
		string(bob): []string{"!group:x"}, string(carol): []string{"!group:x"},
	}))
	ch = f.apply(resp)
	if !slices.Contains(ch.Rooms, "!dm:x") || len(ch.Rooms) != 1 {
		t.Fatalf("Rooms after m.direct change = %v", ch.Rooms)
	}
	if r, _ := f.m.RoomByID("!dm:x"); r.Kind != store.RoomPrivate || r.Section != "rooms" {
		t.Fatalf("former dm = %+v", r)
	}
}

func TestLiveMessagesRelationsAndRedactions(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!general:x")
	resp := newSync()
	join(resp, rid).Timeline.Events = append(join(resp, rid).Timeline.Events, txt(t, "$m1", alice, 1000, "teh fox"))
	ch := f.apply(resp)
	if len(ch.Timeline) != 1 || ch.Timeline[0] != (TimelineChange{Kind: Added, Room: rid, Event: "$m1", TS: f.ts(rid, "$m1"), Live: true}) {
		t.Fatalf("first message: %+v", ch.Timeline)
	}

	resp = newSync()
	edit := mk(t, "m.room.message", "$e1", alice, 2000, nil, map[string]any{
		"msgtype": "m.text", "body": "* the fox",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "the fox"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": "$m1"},
	})
	react := mk(t, "m.reaction", "$r1", bob, 3000, nil, map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$m1", "key": "👍"},
	})
	reply := mk(t, "m.room.message", "$t1", bob, 4000, nil, map[string]any{
		"msgtype": "m.text", "body": "in thread",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$m1", "is_falling_back": true,
			"m.in_reply_to": map[string]any{"event_id": "$m1"}},
	})
	notice := mk(t, "m.room.message", "$n1", bob, 5000, nil, map[string]any{"msgtype": "m.notice", "body": "bot"})
	emote := mk(t, "m.room.message", "$em", bob, 6000, nil, map[string]any{"msgtype": "m.emote", "body": "waves"})
	join(resp, rid).Timeline.Events = []*event.Event{edit, react, reply, notice, emote}
	ch = f.apply(resp)
	m1 := f.ts(rid, "$m1")
	want := []TimelineChange{
		{Kind: Edited, Room: rid, Event: "$m1", TS: m1, Live: true},
		{Kind: ReactionAdded, Room: rid, Event: "$m1", TS: m1, Key: "👍", Sender: bob, Live: true},
		{Kind: Added, Room: rid, Event: "$t1", TS: f.ts(rid, "$t1"), Thread: "$m1", Live: true},
		{Kind: Added, Room: rid, Event: "$n1", TS: f.ts(rid, "$n1"), Live: true},
		{Kind: Added, Room: rid, Event: "$em", TS: f.ts(rid, "$em"), Live: true},
	}
	if !reflect.DeepEqual(ch.Timeline, want) {
		t.Fatalf("Timeline =\n%+v\nwant\n%+v", ch.Timeline, want)
	}
	if e := f.event(rid, "$m1"); e.Body != "the fox" || !strings.Contains(string(e.Content), "teh fox") {
		t.Fatalf("edited original = %+v (body is the edit's, content the original's)", e)
	}
	if e := f.event(rid, "$e1"); e.Body != "" || e.RelType != "m.replace" || e.RelatesTo != "$m1" || e.ThreadRoot != "" {
		t.Fatalf("edit row = %+v", e)
	}
	if e := f.event(rid, "$t1"); e.ThreadRoot != "$m1" || e.RelType != "m.thread" || e.Body != "in thread" {
		t.Fatalf("thread reply row = %+v", e)
	}
	if e := f.event(rid, "$r1"); e.Type != "m.reaction" || e.RelType != "m.annotation" || e.RelatesTo != "$m1" {
		t.Fatalf("reaction row = %+v", e)
	}
	hits, err := f.st.SearchRoom(context.Background(), rid, "fox", 10)
	if err != nil || len(hits) != 1 || hits[0].ID != "$m1" {
		t.Fatalf("search fox = %+v %v (one hit, the original)", hits, err)
	}

	// Redactions: the reaction, the edit (the body goes back), a message.
	resp = newSync()
	redact := func(evID, target id.EventID, ms int64) *event.Event {
		e := mk(t, "m.room.redaction", evID, alice, ms, nil, map[string]any{"redacts": target})
		return e
	}
	join(resp, rid).Timeline.Events = []*event.Event{redact("$x1", "$r1", 7000), redact("$x2", "$e1", 7001), redact("$x3", "$em", 7002)}
	ch = f.apply(resp)
	want = []TimelineChange{
		{Kind: ReactionRemoved, Room: rid, Event: "$m1", TS: m1, Key: "👍", Sender: bob, Live: true},
		{Kind: Edited, Room: rid, Event: "$m1", TS: m1, Live: true},
		{Kind: Redacted, Room: rid, Event: "$em", TS: f.ts(rid, "$em"), Live: true},
	}
	if !reflect.DeepEqual(ch.Timeline, want) {
		t.Fatalf("redactions =\n%+v\nwant\n%+v", ch.Timeline, want)
	}
	if e := f.event(rid, "$m1"); e.Body != "teh fox" {
		t.Fatalf("body after the edit's redaction = %q", e.Body)
	}
	if e := f.event(rid, "$em"); !e.Redacted || e.Body != "" || string(e.Content) != "{}" {
		t.Fatalf("redacted message = %+v", e)
	}
	if _, ok, _ := f.st.GetEvent(context.Background(), rid, "$x1"); ok {
		t.Fatal("redaction events are not stored")
	}

	// A re-delivery of the redacted message (as the server sends it)
	// changes nothing.
	resp = newSync()
	again := txt(t, "$em", bob, 6000, "")
	again.Unsigned.RedactedBecause = redact("$x3", "$em", 7002)
	join(resp, rid).Timeline.Events = []*event.Event{again, txt(t, "$m1", alice, 1000, "teh fox")}
	if ch := f.apply(resp); len(ch.Timeline) != 0 {
		t.Fatalf("re-delivery: %+v", ch.Timeline)
	}
}

func TestLimitedTimelineRecordsGap(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	resp := newSync()
	jr := join(resp, rid)
	jr.Timeline = mautrix.SyncTimeline{Limited: true, PrevBatch: "p1"}
	jr.Timeline.Events = []*event.Event{memberEv(t, alice, "Alice"), txt(t, "$a", alice, 1000, "a"), txt(t, "$b", alice, 2000, "b")}
	jr.Timeline.Events[0].ID = "$join"
	f.apply(resp)
	gaps, err := f.st.Gaps(context.Background(), rid)
	if err != nil || len(gaps) != 1 || gaps[0].PrevBatch != "p1" || gaps[0].AfterTS != f.ts(rid, "$join") {
		t.Fatalf("gaps = %+v %v", gaps, err)
	}
	if !(gaps[0].AfterTS < f.ts(rid, "$a") && f.ts(rid, "$a") < f.ts(rid, "$b")) {
		t.Fatalf("gap %s not before the new events %s %s", gaps[0].AfterTS, f.ts(rid, "$a"), f.ts(rid, "$b"))
	}
	if got := f.m.DisplayName(rid, alice); got != "Alice" {
		t.Fatalf("member from the timeline: %q", got)
	}

	// An unlimited timeline adds no gap.
	resp = newSync()
	join(resp, rid).Timeline.Events = []*event.Event{txt(t, "$c", alice, 3000, "c")}
	f.apply(resp)
	if gaps, _ := f.st.Gaps(context.Background(), rid); len(gaps) != 1 {
		t.Fatalf("gaps after an unlimited sync = %+v", gaps)
	}
}

func TestBackfillMintsOlderTSInOrder(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	resp := newSync()
	join(resp, rid).Timeline.Events = []*event.Event{txt(t, "$live", alice, 10_000, "live")}
	f.apply(resp)

	// Newest first, as /messages?dir=b returns them. $b3 is older than
	// the oldest stored event, so it and everything after it in the page
	// go before the oldest, $b2 too although its origin is later than
	// $b3's (bumped).
	evts := []*event.Event{
		txt(t, "$b3", bob, 9_000, "three"),
		txt(t, "$b2", bob, 9_500, "two"),
		mk(t, "m.room.message", "$e", bob, 4_000, nil, map[string]any{
			"msgtype": "m.text", "body": "* one!", "m.new_content": map[string]any{"body": "one!"},
			"m.relates_to": map[string]any{"rel_type": "m.replace", "event_id": "$b1"},
		}),
		txt(t, "$b1", bob, 3_000, "one"),
	}
	member := memberEv(t, bob, "Bobby")
	ch, err := f.m.Backfill(context.Background(), rid, evts, []*event.Event{member})
	if err != nil {
		t.Fatal(err)
	}
	b1, b2, b3, live := f.ts(rid, "$b1"), f.ts(rid, "$b2"), f.ts(rid, "$b3"), f.ts(rid, "$live")
	if !(b1 < f.ts(rid, "$e") && f.ts(rid, "$e") < b2 && b2 < b3 && b3 < live) {
		t.Fatalf("order: b1 %s e %s b2 %s b3 %s live %s", b1, f.ts(rid, "$e"), b2, b3, live)
	}
	want := []TimelineChange{
		{Kind: Added, Room: rid, Event: "$b3", TS: b3},
		{Kind: Added, Room: rid, Event: "$b2", TS: b2},
		{Kind: Added, Room: rid, Event: "$b1", TS: b1},
	}
	if !reflect.DeepEqual(ch.Timeline, want) {
		t.Fatalf("Timeline = %+v\nwant %+v", ch.Timeline, want)
	}
	if e := f.event(rid, "$b1"); e.Body != "one!" {
		t.Fatalf("an edit backfilled before its original applies to it: %q", e.Body)
	}
	if got := f.m.DisplayName(rid, bob); got != "Bobby" || !slices.Equal(ch.Names, []id.UserID{bob}) {
		t.Fatalf("backfill state names senders: %q %v", got, ch.Names)
	}
	// A backfilled member event never replaces a known one.
	resp = newSync()
	join(resp, rid).State.Events = []*event.Event{memberEv(t, bob, "Robert")}
	f.apply(resp)
	if _, err := f.m.Backfill(context.Background(), rid, nil, []*event.Event{member}); err != nil {
		t.Fatal(err)
	}
	if got := f.m.DisplayName(rid, bob); got != "Robert" {
		t.Fatalf("name after backfill state = %q", got)
	}
	if r, _, _ := f.st.GetRoom(context.Background(), rid); r.LastEventTS != live {
		t.Fatalf("backfill moved LastEventTS: %+v", r)
	}
}

func TestLateDecryption(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	enc := mk(t, "m.room.encrypted", "$enc", alice, 1000, nil, map[string]any{
		"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "AAA", "session_id": "s", "sender_key": "k",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$root"},
	})
	resp := newSync()
	join(resp, rid).Timeline.Events = []*event.Event{enc}
	ch := f.apply(resp)
	ts := f.ts(rid, "$enc")
	if len(ch.Timeline) != 1 || ch.Timeline[0].Kind != Added || ch.Timeline[0].Thread != "$root" {
		t.Fatalf("encrypted: %+v", ch.Timeline)
	}
	if e := f.event(rid, "$enc"); e.Type != "m.room.encrypted" || e.Decrypted || e.Body != "" || e.ThreadRoot != "$root" {
		t.Fatalf("stored encrypted = %+v", e)
	}
	if rs := f.m.Snapshot().ReadStates[f.roomAlias(rid)]; rs.HasUnread {
		t.Fatalf("a thread reply made the room unread: %+v", rs)
	}

	dec := mk(t, "m.room.message", "$enc", alice, 1000, nil, map[string]any{
		"msgtype": "m.text", "body": "secret words",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$root"},
	})
	dec.Mautrix.WasEncrypted = true
	ch, err := f.m.ApplyDecrypted(context.Background(), session.LateDecrypted{Room: rid, Events: []*event.Event{dec}})
	if err != nil {
		t.Fatal(err)
	}
	want := []TimelineChange{{Kind: Edited, Room: rid, Event: "$enc", TS: ts, Thread: "$root", Live: true}}
	if !reflect.DeepEqual(ch.Timeline, want) {
		t.Fatalf("ApplyDecrypted = %+v, want %+v", ch.Timeline, want)
	}
	if e := f.event(rid, "$enc"); e.Type != "m.room.message" || !e.Decrypted || e.Body != "secret words" || e.TS != ts {
		t.Fatalf("decrypted row = %+v", e)
	}
}

func TestReceiptsAndUnread(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	a := f.roomAlias(rid)
	receipt := func(user id.UserID, ev id.EventID, typ, thread string) *event.Event {
		r := map[string]any{"ts": 1}
		if thread != "" {
			r["thread_id"] = thread
		}
		return mk(t, "m.receipt", "", "", 0, nil, map[string]any{
			string(ev): map[string]any{typ: map[string]any{string(user): r}},
		})
	}
	readState := func() core.ReadState { return f.m.Snapshot().ReadStates[a] }

	resp := newSync()
	jr := join(resp, rid)
	jr.Timeline.Events = []*event.Event{txt(t, "$1", alice, 1000, "hi")}
	jr.UnreadNotifications = &mautrix.UnreadNotificationCounts{HighlightCount: 2, NotificationCount: 1}
	ch := f.apply(resp)
	if rs := readState(); !rs.HasUnread || rs.MentionCount != 2 || rs.LastReadTS != "" || !slices.Contains(ch.ReadStates, rid) {
		t.Fatalf("after a message: %+v (changes %v)", rs, ch.ReadStates)
	}

	// Own receipt from another device, and someone else's.
	resp = newSync()
	jr = join(resp, rid)
	jr.Ephemeral.Events = []*event.Event{receipt(me, "$1", "m.read", ""), receipt(bob, "$1", "m.read", "")}
	jr.UnreadNotifications = &mautrix.UnreadNotificationCounts{}
	ch = f.apply(resp)
	t1 := f.ts(rid, "$1")
	if rs := readState(); rs != (core.ReadState{LastReadTS: t1}) {
		t.Fatalf("after own receipt: %+v", rs)
	}
	if !reflect.DeepEqual(ch.OwnReceipts, []Receipt{{Room: rid, Event: "$1", TS: t1}}) || !slices.Equal(ch.ReadStates, []id.RoomID{rid}) {
		t.Fatalf("changes: %+v %v", ch.OwnReceipts, ch.ReadStates)
	}
	if r, ok, err := f.st.GetReceipt(context.Background(), rid, "", bob); err != nil || !ok || r.TS != t1 {
		t.Fatalf("bob's receipt = %+v %v %v", r, ok, err)
	}

	// Own messages and notices are never unread.
	resp = newSync()
	join(resp, rid).Timeline.Events = []*event.Event{
		txt(t, "$2", alice, 2000, "before mine"),
		txt(t, "$3", me, 3000, "mine"),
		mk(t, "m.room.message", "$4", bob, 4000, nil, map[string]any{"msgtype": "m.notice", "body": "bot"}),
		mk(t, "m.reaction", "$5", bob, 5000, nil, map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$3", "key": "x"}}),
	}
	ch = f.apply(resp)
	if rs := readState(); rs.HasUnread || len(ch.ReadStates) != 0 {
		t.Fatalf("own message, notice, reaction: %+v %v", rs, ch.ReadStates)
	}
	resp = newSync()
	join(resp, rid).Timeline.Events = []*event.Event{txt(t, "$6", bob, 6000, "new")}
	f.apply(resp)
	if rs := readState(); !rs.HasUnread {
		t.Fatalf("a message after own one: %+v", rs)
	}

	// m.read.private moves it on; an older m.read does not move it back.
	resp = newSync()
	join(resp, rid).Ephemeral.Events = []*event.Event{receipt(me, "$6", "m.read.private", ""), receipt(me, "$2", "m.read", "")}
	ch = f.apply(resp)
	if rs := readState(); rs.HasUnread || rs.LastReadTS != f.ts(rid, "$6") || len(ch.OwnReceipts) != 1 {
		t.Fatalf("private receipt: %+v %+v", rs, ch.OwnReceipts)
	}

	// Threaded receipts, and the unthreaded one covering threads.
	resp = newSync()
	join(resp, rid).Ephemeral.Events = []*event.Event{receipt(me, "$1", "m.read", "$root")}
	ch = f.apply(resp)
	if got, ok := f.m.ThreadReceipt(rid, "$root"); !ok || got.Event != "$6" || got.Thread != "$root" {
		t.Fatalf("ThreadReceipt = %+v %v (the later unthreaded receipt)", got, ok)
	}
	if !reflect.DeepEqual(ch.OwnReceipts, []Receipt{{Room: rid, Thread: "$root", Event: "$1", TS: t1}}) {
		t.Fatalf("threaded own receipt change: %+v", ch.OwnReceipts)
	}

	// Marked unread.
	resp = newSync()
	join(resp, rid).AccountData.Events = []*event.Event{mk(t, "m.marked_unread", "", "", 0, nil, map[string]any{"unread": true})}
	f.apply(resp)
	if rs := readState(); !rs.HasUnread {
		t.Fatalf("marked unread: %+v", rs)
	}
}

func TestReceiptOnUnknownEventUsesServerCount(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	resp := newSync()
	jr := join(resp, rid)
	jr.Timeline.Events = []*event.Event{txt(t, "$1", alice, 1000, "hi")}
	jr.Ephemeral.Events = []*event.Event{mk(t, "m.receipt", "", "", 0, nil, map[string]any{
		"$old": map[string]any{"m.read": map[string]any{string(me): map[string]any{"ts": 1}}},
	})}
	jr.UnreadNotifications = &mautrix.UnreadNotificationCounts{NotificationCount: 0}
	f.apply(resp)
	if rs := f.m.Snapshot().ReadStates[f.roomAlias(rid)]; rs.HasUnread || rs.LastReadTS != "" {
		t.Fatalf("read state = %+v", rs)
	}
	// Backfilling the receipt's event gives it its TS.
	ch, err := f.m.Backfill(context.Background(), rid, []*event.Event{txt(t, "$old", alice, 500, "old")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rs := f.m.Snapshot().ReadStates[f.roomAlias(rid)]; !rs.HasUnread || rs.LastReadTS != f.ts(rid, "$old") || len(ch.OwnReceipts) != 1 {
		t.Fatalf("after backfill: %+v %+v", rs, ch.OwnReceipts)
	}
}

func TestFarFutureOriginIsCapped(t *testing.T) {
	f := newFixture(t)
	now := time.UnixMilli(1_800_000_000_000)
	f.m.now = func() time.Time { return now }
	const rid = id.RoomID("!r:x")
	resp := newSync()
	join(resp, rid).Timeline.Events = []*event.Event{
		txt(t, "$future", alice, now.Add(365*24*time.Hour).UnixMilli(), "from the future"),
		txt(t, "$next", alice, now.UnixMilli(), "now"),
	}
	f.apply(resp)
	limit := fmt.Sprintf("%010d", now.Add(maxFuture).Unix())
	if got := f.ts(rid, "$future"); got[:10] != limit {
		t.Fatalf("future TS = %s, want second %s", got, limit)
	}
	if got := f.ts(rid, "$next"); got[:10] != limit {
		t.Fatalf("next TS = %s, want just after the capped one", got)
	}
}

func TestTypingNamesAndDisplayNames(t *testing.T) {
	f := newFixture(t)
	const rid, other = id.RoomID("!r:x"), id.RoomID("!o:x")
	resp := newSync()
	jr := join(resp, rid)
	jr.State.Events = []*event.Event{memberEv(t, alice, "Alice")}
	jr.Ephemeral.Events = []*event.Event{mk(t, "m.typing", "", "", 0, nil, map[string]any{"user_ids": []string{string(alice), string(me)}})}
	join(resp, other).State.Events = []*event.Event{memberEv(t, alice, "Ally")}
	ch := f.apply(resp)
	if !reflect.DeepEqual(ch.Typing, map[id.RoomID][]id.UserID{rid: {alice}}) {
		t.Fatalf("Typing = %v", ch.Typing)
	}
	if !slices.Equal(ch.Names, []id.UserID{alice}) {
		t.Fatalf("Names = %v", ch.Names)
	}
	for _, c := range []struct {
		room id.RoomID
		user id.UserID
		want string
	}{
		{rid, alice, "Alice"}, {other, alice, "Ally"},
		{"", alice, "Ally"},     // global: first room by ID
		{"!z:x", alice, "Ally"}, // a room without the member
		{rid, "@zed:y", "zed"},  // unknown: localpart
		{"", "not-an-id", "not-an-id"},
	} {
		if got := f.m.DisplayName(c.room, c.user); got != c.want {
			t.Errorf("DisplayName(%s, %s) = %q, want %q", c.room, c.user, got, c.want)
		}
	}
	resp = newSync()
	join(resp, rid).Timeline.Events = []*event.Event{memberEv(t, alice, "Alicia")}
	join(resp, rid).Ephemeral.Events = []*event.Event{mk(t, "m.typing", "", "", 0, nil, map[string]any{"user_ids": []string{}})}
	ch = f.apply(resp)
	if !slices.Equal(ch.Names, []id.UserID{alice}) || f.m.DisplayName(rid, alice) != "Alicia" {
		t.Fatalf("rename: %v %q", ch.Names, f.m.DisplayName(rid, alice))
	}
	if got, ok := ch.Typing[rid]; !ok || len(got) != 0 {
		t.Fatalf("typing stopped: %v", ch.Typing)
	}
}

func TestResolver(t *testing.T) {
	f := newFixture(t)
	f.apply(world(t))
	r := f.m.Resolver()
	a := r.UserAlias(alice)
	if a != f.userAlias(alice) {
		t.Fatalf("UserAlias = %q", a)
	}
	if u, ok := r.MXID(a); !ok || u != alice {
		t.Fatalf("MXID = %v %v", u, ok)
	}
	if u := r.UserAlias("@new:x"); u == "" {
		t.Fatal("UserAlias mints for a new user")
	} else if back, ok := r.MXID(u); !ok || back != "@new:x" {
		t.Fatalf("MXID of a minted alias = %v %v", back, ok)
	}
	ra := r.RoomAlias("!general:x")
	if rid, ok := r.RoomID(ra); !ok || rid != "!general:x" {
		t.Fatalf("RoomID = %v %v", rid, ok)
	}
	if r.RoomName("!general:x") != "general" || r.DisplayName(bob) != "Bob" {
		t.Fatalf("names: %q %q", r.RoomName("!general:x"), r.DisplayName(bob))
	}
	if _, ok := r.MXID("Cnotauser00"); ok {
		t.Fatal("MXID of a room alias")
	}
}

func TestRestartYieldsSameSnapshot(t *testing.T) {
	f := newFixture(t)
	f.apply(world(t))
	resp := newSync()
	jr := join(resp, "!general:x")
	jr.Timeline.Events = []*event.Event{txt(t, "$1", alice, 1000, "hi"), txt(t, "$2", bob, 2000, "yo")}
	jr.Ephemeral.Events = []*event.Event{mk(t, "m.receipt", "", "", 0, nil, map[string]any{
		"$1": map[string]any{"m.read": map[string]any{string(me): map[string]any{"ts": 1}}},
	})}
	jr.UnreadNotifications = &mautrix.UnreadNotificationCounts{HighlightCount: 1, NotificationCount: 1}
	join(resp, "!secret:x").State.Events = []*event.Event{state(t, "m.room.name", "", alice, map[string]any{"name": ""}),
		memberEv(t, carol, "Carol")}
	f.apply(resp)
	before := f.m.Snapshot()
	if got := before.ChannelNames[f.roomAlias("!secret:x")]; got != "Carol" {
		t.Fatalf("unnamed room = %q, want its member's name", got)
	}

	f.m = f.reopen()
	after := f.m.Snapshot()
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("snapshot after restart differs:\nbefore %+v\nafter  %+v", before, after)
	}
	if got, ok := f.m.ThreadReceipt("!general:x", "$t"); !ok || got.Event != "$1" {
		t.Fatalf("receipt after restart = %+v %v", got, ok)
	}
	// An unrelated change after the restart keeps the names.
	resp = newSync()
	join(resp, "!dm:x").Timeline.Events = []*event.Event{txt(t, "$3", alice, 3000, "x")}
	join(resp, "!eng:x").Timeline.Events = []*event.Event{txt(t, "$4", alice, 3000, "x")}
	if ch := f.apply(resp); len(ch.Rooms) != 0 {
		t.Fatalf("Rooms after an unrelated change = %v", ch.Rooms)
	}
}

// TestSnapshotIsFresh checks that a snapshot's maps are the caller's.
func TestSnapshotIsFresh(t *testing.T) {
	f := newFixture(t)
	f.apply(world(t))
	s1 := f.m.Snapshot()
	for k := range s1.ChannelNames {
		s1.ChannelNames[k] = "changed"
	}
	s1.Channels[0].Name = "changed"
	s2 := f.m.Snapshot()
	if s2.Channels[0].Name == "changed" || slices.Contains(mapsValues(s2.ChannelNames), "changed") {
		t.Fatal("a snapshot shares state with the model")
	}
}

func mapsValues(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}

func TestLeaveAndInvite(t *testing.T) {
	f := newFixture(t)
	f.apply(world(t))
	resp := newSync()
	resp.Rooms.Leave["!general:x"] = &mautrix.SyncLeftRoom{}
	resp.Rooms.Invite["!inv:x"] = &mautrix.SyncInvitedRoom{State: mautrix.SyncEventsList{Events: []*event.Event{
		state(t, "m.room.member", string(me), carol, map[string]any{"membership": "invite", "is_direct": true}),
		state(t, "m.room.member", string(carol), carol, map[string]any{"membership": "join", "displayname": "Carol"}),
	}}}
	ch := f.apply(resp)
	if !slices.Equal(ch.Left, []id.RoomID{"!general:x"}) || len(ch.Joined) != 0 {
		t.Fatalf("Left %v Joined %v", ch.Left, ch.Joined)
	}
	if !slices.Contains(ch.Rooms, "!general:x") || !slices.Contains(ch.Rooms, "!inv:x") {
		t.Fatalf("Rooms = %v", ch.Rooms)
	}
	for _, c := range f.m.Snapshot().Channels {
		if c.ID == f.roomAlias("!general:x") || c.ID == f.roomAlias("!inv:x") {
			t.Fatalf("left or invited room in the sidebar: %+v", c)
		}
	}
	if r, ok := f.m.RoomByID("!inv:x"); !ok || r.Kind != store.RoomDM || r.DMUser != carol || r.Name != "Carol" {
		t.Fatalf("invite = %+v %v", r, ok)
	}
}

// TestOthersReceiptsAreReported: other users' receipts that move forward
// are in Changes.Receipts with their thread and read time stored; one
// that would move back, and the account's own, are not.
func TestOthersReceiptsAreReported(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	receipts := func(rs ...[4]string) *mautrix.RespSync { // user, event, thread, ts
		content := map[string]map[string]map[string]any{}
		for _, r := range rs {
			c := map[string]any{"ts": json.Number(r[3])}
			if r[2] != "" {
				c["thread_id"] = r[2]
			}
			if content[r[1]] == nil {
				content[r[1]] = map[string]map[string]any{"m.read": {}}
			}
			content[r[1]]["m.read"][r[0]] = c
		}
		resp := newSync()
		join(resp, rid).Ephemeral.Events = []*event.Event{mk(t, "m.receipt", "", "", 0, nil, content)}
		return resp
	}
	resp := newSync()
	join(resp, rid).Timeline.Events = []*event.Event{txt(t, "$1", alice, 1000, "one"), txt(t, "$2", alice, 2000, "two")}
	f.apply(resp)
	t1, t2 := f.ts(rid, "$1"), f.ts(rid, "$2")

	ch := f.apply(receipts([4]string{string(bob), "$2", "main", "5000"}, [4]string{string(me), "$2", "", "5000"}))
	if want := []UserReceipt{{Room: rid, User: bob, Thread: "main", Event: "$2", TS: t2}}; !reflect.DeepEqual(ch.Receipts, want) {
		t.Fatalf("Receipts = %+v, want %+v", ch.Receipts, want)
	}
	if r, _, _ := f.st.GetReceipt(context.Background(), rid, "main", bob); r.ReadAt != 5000 {
		t.Fatalf("stored read time = %d", r.ReadAt)
	}
	// Back to $1: dropped.
	if ch := f.apply(receipts([4]string{string(bob), "$1", "main", "6000"})); len(ch.Receipts) != 0 {
		t.Fatalf("an older receipt moved: %+v", ch.Receipts)
	}
	if r, _, _ := f.st.GetReceipt(context.Background(), rid, "main", bob); r.EventID != "$2" || r.ReadAt != 5000 {
		t.Fatalf("after an older receipt: %+v", r)
	}
	// Unthreaded on $1 is another receipt (another thread_id); on an
	// event not stored, it has no TS yet.
	ch = f.apply(receipts([4]string{string(bob), "$1", "", "7000"}, [4]string{string(alice), "$later", "", "7000"}))
	if want := []UserReceipt{{Room: rid, User: bob, Event: "$1", TS: t1}, {Room: rid, User: alice, Event: "$later"}}; !reflect.DeepEqual(ch.Receipts, want) {
		t.Fatalf("Receipts = %+v, want %+v", ch.Receipts, want)
	}
}
