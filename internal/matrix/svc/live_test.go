package svc

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/config"
	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/sidebar"
)

const liveDM id.RoomID = "!dm:example.org"

// liveFixture is a Backend over a real room model and store, running Run
// on an events channel the test feeds.
type liveFixture struct {
	t   *testing.T
	b   *Backend
	m   *rooms.Model
	st  *store.Store
	ids *ident.Mapper

	events chan session.Event
	done   chan error
	cancel context.CancelFunc

	mu   sync.Mutex
	sent []tea.Msg
	// notes are the notifications delivered, "title|body".
	notes chan string
	// active is the conversation on screen, for notifications.
	active string
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ids := ident.New(st)
	m, err := rooms.New(ctx, st, ids, self)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	f := &liveFixture{t: t, m: m, st: st, ids: ids, events: make(chan session.Event), done: make(chan error, 1), notes: make(chan string, 16)}
	f.b = newBackend(Deps{Account: "acct", Store: st, IDs: ids, Rooms: m}, m, nil, self)
	f.b.SetNotifications(Notifications{
		Notifier: notifierFunc(func(title, body string) error { f.notes <- title + "|" + body; return nil }),
		Config:   config.Notifications{Enabled: true, OnMention: true, OnDM: true},
		ActiveChannelID: func() string {
			f.mu.Lock()
			defer f.mu.Unlock()
			return f.active
		},
	})
	return f
}

type notifierFunc func(title, body string) error

func (n notifierFunc) Notify(title, body string) error { return n(title, body) }

// start runs Run until the test ends.
func (f *liveFixture) start() {
	ctx, cancel := context.WithCancel(context.Background())
	f.cancel = cancel
	go func() {
		f.done <- f.b.Run(ctx, f.events, func(m tea.Msg) {
			f.mu.Lock()
			f.sent = append(f.sent, m)
			f.mu.Unlock()
		})
	}()
	f.t.Cleanup(func() {
		cancel()
		if err := <-f.done; err != nil && !errors.Is(err, context.Canceled) {
			f.t.Errorf("Run: %v", err)
		}
	})
}

// startup runs Run and applies resp as the first sync, returning
// StartupMsgs' messages.
func (f *liveFixture) startup(resp *mautrix.RespSync) []tea.Msg {
	f.t.Helper()
	f.start()
	go func() { f.events <- session.SyncBatch{Resp: resp, Initial: true} }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	msgs, err := f.b.StartupMsgs(ctx)
	if err != nil {
		f.t.Fatalf("StartupMsgs: %v", err)
	}
	f.flush()
	if got := f.take(); len(got) != 0 {
		f.t.Fatalf("the first sync sent %+v", got)
	}
	return msgs
}

// flush returns once Run has handled every event given before: Run takes
// the next event only when it is done with the last one.
func (f *liveFixture) flush() {
	f.t.Helper()
	select {
	case f.events <- session.SyncBatch{}:
	case <-time.After(5 * time.Second):
		f.t.Fatal("Run is not taking events")
	}
}

// step delivers ev and returns what Run sent for it.
func (f *liveFixture) step(ev session.Event) []tea.Msg {
	f.t.Helper()
	f.take()
	f.events <- ev
	f.flush()
	return f.take()
}

func (f *liveFixture) sync(resp *mautrix.RespSync) []tea.Msg {
	return f.step(session.SyncBatch{Resp: resp})
}

func (f *liveFixture) take() []tea.Msg {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.sent
	f.sent = nil
	return out
}

func (f *liveFixture) ts(room id.RoomID, ev id.EventID) string {
	f.t.Helper()
	ts, ok, err := f.ids.TS(context.Background(), room, ev)
	if err != nil || !ok {
		f.t.Fatalf("TS of %s: %v %v", ev, ok, err)
	}
	return ts
}

func (f *liveFixture) roomAlias(room id.RoomID) string { return f.m.Resolver().RoomAlias(room) }
func (f *liveFixture) userAlias(u id.UserID) string    { return f.m.Resolver().UserAlias(u) }

// msgItem is the item of a message as the UI gets it.
func (f *liveFixture) msgItem(room id.RoomID, ev id.EventID, sender id.UserID, text string) core.MessageItem {
	ts := f.ts(room, ev)
	return core.MessageItem{
		TS: ts, UserID: f.userAlias(sender), UserName: f.m.DisplayName(room, sender),
		Timestamp: f.b.formatTS(ts), Text: text,
	}
}

func (f *liveFixture) expect(what string, got []tea.Msg, want ...tea.Msg) {
	f.t.Helper()
	if !reflect.DeepEqual(got, want) {
		f.t.Fatalf("%s sent\n%#v\nwant\n%#v", what, got, want)
	}
}

// noNote fails if a notification was delivered.
func (f *liveFixture) noNote(what string) {
	f.t.Helper()
	select {
	case n := <-f.notes:
		f.t.Fatalf("%s notified %q", what, n)
	case <-time.After(50 * time.Millisecond):
	}
}

func (f *liveFixture) note(what string) string {
	f.t.Helper()
	select {
	case n := <-f.notes:
		return n
	case <-time.After(5 * time.Second):
		f.t.Fatalf("%s: no notification", what)
	}
	return ""
}

// lmk builds an event as one parsed from a /sync response.
func lmk(t *testing.T, typ string, evID id.EventID, sender id.UserID, ms int64, stateKey *string, content any) *event.Event {
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

func lstate(t *testing.T, typ, key string, sender id.UserID, content any) *event.Event {
	return lmk(t, typ, id.EventID("$state-"+typ+"-"+key), sender, 1, &key, content)
}

func lmember(t *testing.T, user id.UserID, name string) *event.Event {
	return lstate(t, "m.room.member", string(user), user, map[string]any{"membership": "join", "displayname": name})
}

func ltxt(t *testing.T, evID id.EventID, sender id.UserID, ms int64, body string) *event.Event {
	return lmk(t, "m.room.message", evID, sender, ms, nil, map[string]any{"msgtype": "m.text", "body": body})
}

func lsync() *mautrix.RespSync {
	return &mautrix.RespSync{Rooms: mautrix.RespSyncRooms{
		Join:   map[id.RoomID]*mautrix.SyncJoinedRoom{},
		Invite: map[id.RoomID]*mautrix.SyncInvitedRoom{},
		Leave:  map[id.RoomID]*mautrix.SyncLeftRoom{},
	}}
}

func ljoin(resp *mautrix.RespSync, rid id.RoomID) *mautrix.SyncJoinedRoom {
	jr := resp.Rooms.Join[rid]
	if jr == nil {
		jr = &mautrix.SyncJoinedRoom{}
		resp.Rooms.Join[rid] = jr
	}
	return jr
}

// timeline is a sync with events in room's timeline.
func timeline(room id.RoomID, evs ...*event.Event) *mautrix.RespSync {
	resp := lsync()
	ljoin(resp, room).Timeline.Events = evs
	return resp
}

// liveWorld is a first sync: the public room general with a message from
// alice and a DM with alice, whose message is older.
func liveWorld(t *testing.T) *mautrix.RespSync {
	resp := lsync()
	resp.AccountData.Events = append(resp.AccountData.Events, lmk(t, "m.direct", "", "", 0, nil, map[string]any{
		string(alice): []string{string(liveDM)},
	}))
	g := ljoin(resp, general)
	g.State.Events = []*event.Event{
		lstate(t, "m.room.name", "", alice, map[string]any{"name": "general"}),
		lstate(t, "m.room.join_rules", "", alice, map[string]any{"join_rule": "public"}),
		lmember(t, alice, "Alice"), lmember(t, bob, "Bob"), lmember(t, self, "Me"),
	}
	g.Timeline.Events = []*event.Event{ltxt(t, "$g1", alice, 2000, "welcome")}
	dm := ljoin(resp, liveDM)
	dm.Summary = mautrix.LazyLoadSummary{Heroes: []id.UserID{alice}, JoinedMemberCount: intPtr(2)}
	dm.State.Events = []*event.Event{lmember(t, alice, "Alice"), lmember(t, self, "Me")}
	dm.Timeline.Events = []*event.Event{ltxt(t, "$d1", alice, 1000, "hi")}
	return resp
}

func intPtr(n int) *int { return &n }

func TestStartupMsgsAfterTheFirstSync(t *testing.T) {
	f := newLiveFixture(t)
	f.start()
	// Connecting before the first sync is not sent: the workspace is not
	// there yet.
	f.events <- session.Connection{State: session.Connecting}
	f.flush()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("before startup: %+v", got)
	}
	got := make(chan []tea.Msg, 1)
	go func() {
		msgs, err := f.b.StartupMsgs(context.Background())
		if err != nil {
			t.Errorf("StartupMsgs: %v", err)
		}
		got <- msgs
	}()
	select {
	case <-got:
		t.Fatal("StartupMsgs returned before the first sync")
	case <-time.After(50 * time.Millisecond):
	}
	f.events <- session.SyncBatch{Resp: liveWorld(t), Initial: true}
	msgs := <-got
	f.flush()
	if s := f.take(); len(s) != 0 {
		t.Fatalf("the first sync sent %+v", s)
	}

	snap := f.m.Snapshot()
	ready, ok := msgs[0].(ui.WorkspaceReadyMsg)
	if len(msgs) != 2 || !ok {
		t.Fatalf("StartupMsgs = %#v", msgs)
	}
	if msgs[1] != (ui.ConnectionStateMsg{State: 1}) {
		t.Fatalf("connection = %#v", msgs[1])
	}
	gen := f.roomAlias(general)
	if ready.TeamID != f.b.TeamID() || ready.TeamName != "acct" || ready.UserID != f.userAlias(self) ||
		!ready.InitialActive || ready.LastChannelID != gen {
		t.Fatalf("ready = %+v", ready)
	}
	if !reflect.DeepEqual(ready.Channels, snap.Channels) || !reflect.DeepEqual(ready.FinderItems, snap.Finder) ||
		!reflect.DeepEqual(ready.ChannelNames, snap.ChannelNames) || !reflect.DeepEqual(ready.UserNames, snap.UserNames) {
		t.Fatalf("ready rows = %+v\nsnapshot %+v", ready, snap)
	}
	if len(ready.Channels) != 2 || ready.Channels[0].ID != gen || ready.Channels[1].Type != "dm" {
		t.Fatalf("Channels = %+v", ready.Channels)
	}
	if p := ready.SectionsProvider; p == nil || !p.Ready() || !reflect.DeepEqual(p.OrderedSlackSections(), snap.Sections) {
		t.Fatalf("SectionsProvider = %v", p)
	}
	// The maps are the UI's: writing them changes nothing the backend
	// reads.
	ready.UserNames["x"], ready.ChannelNames["x"] = "y", "y"
	ready.SectionsProvider.OrderedSlackSections()[0].Name = "changed"
	if _, ok := f.m.Snapshot().UserNames["x"]; ok || ready.SectionsProvider.OrderedSlackSections()[0].Name == "changed" {
		t.Fatal("startup maps are shared")
	}
	f.noNote("the first sync")

	// From now on the connection is told.
	f.expect("disconnect", f.step(session.Connection{State: session.Disconnected, Err: errors.New("x")}), ui.ConnectionStateMsg{State: 2})
	f.expect("reconnect", f.step(session.Connection{State: session.Connecting}), ui.ConnectionStateMsg{State: 0})
	f.expect("connected", f.step(session.Connection{State: session.Connected}), ui.ConnectionStateMsg{State: 1})
}

func TestStartupWhenTheFirstSyncFails(t *testing.T) {
	f := newLiveFixture(t)
	f.start()
	f.events <- session.Connection{State: session.Connecting}
	go func() { f.events <- session.Connection{State: session.Disconnected, Err: errors.New("offline")} }()
	msgs, err := f.b.StartupMsgs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[1] != (ui.ConnectionStateMsg{State: 2}) {
		t.Fatalf("offline startup = %#v", msgs)
	}
	// The initial sync, once it comes, is told (it is not in the startup
	// snapshot), but its messages do not notify.
	got := f.step(session.SyncBatch{Resp: liveWorld(t), Initial: true})
	var news int
	for _, m := range got {
		if _, ok := m.(ui.NewMessageMsg); ok {
			news++
		}
	}
	if news != 2 {
		t.Fatalf("initial sync after an offline start sent %#v", got)
	}
	f.noNote("the initial sync")
}

func TestStartupMsgsHonoursItsContext(t *testing.T) {
	f := newLiveFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.b.StartupMsgs(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("StartupMsgs = %v", err)
	}
}

func TestLiveTimelineChanges(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	gen := f.roomAlias(general)

	// A message, and its edit.
	got := f.sync(timeline(general, ltxt(t, "$m1", bob, 3000, "teh fox")))
	f.expect("a message", got, ui.NewMessageMsg{ChannelID: gen, Message: f.msgItem(general, "$m1", bob, "teh fox")})
	f.noNote("a plain channel message")

	edit := lmk(t, "m.room.message", "$e1", bob, 4000, nil, map[string]any{
		"msgtype": "m.text", "body": "* the fox",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "the fox"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": "$m1"},
	})
	edited := f.msgItem(general, "$m1", bob, "the fox")
	edited.IsEdited = true
	f.expect("an edit", f.sync(timeline(general, edit)), ui.NewMessageMsg{ChannelID: gen, Message: edited, Changed: true})

	// A thread reply carries its root's TS; the UI bumps the root's
	// reply count itself.
	reply := lmk(t, "m.room.message", "$t1", alice, 5000, nil, map[string]any{
		"msgtype": "m.text", "body": "in thread",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$m1", "is_falling_back": true,
			"m.in_reply_to": map[string]any{"event_id": "$m1"}},
	})
	got = f.sync(timeline(general, reply))
	want := f.msgItem(general, "$t1", alice, "in thread")
	want.ThreadTS = f.ts(general, "$m1")
	f.expect("a thread reply", got, ui.NewMessageMsg{ChannelID: gen, Message: want})

	// Reactions, by Slack shortcode.
	react := lmk(t, "m.reaction", "$r1", alice, 6000, nil, map[string]any{
		"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": "$m1", "key": "👍"},
	})
	m1 := f.ts(general, "$m1")
	f.expect("a reaction", f.sync(timeline(general, react)),
		ui.ReactionAddedMsg{ChannelID: gen, MessageTS: m1, UserID: f.userAlias(alice), Emoji: "+1"})
	redact := func(evID, target id.EventID, ms int64) *event.Event {
		return lmk(t, "m.room.redaction", evID, alice, ms, nil, map[string]any{"redacts": target})
	}
	f.expect("a reaction redacted", f.sync(timeline(general, redact("$x1", "$r1", 7000))),
		ui.ReactionRemovedMsg{ChannelID: gen, MessageTS: m1, UserID: f.userAlias(alice), Emoji: "+1"})

	// A redacted message goes; a redacted root with replies stays as a
	// tombstone.
	f.sync(timeline(general, ltxt(t, "$m2", bob, 8000, "oops")))
	m2 := f.ts(general, "$m2")
	f.expect("a redaction", f.sync(timeline(general, redact("$x2", "$m2", 9000))),
		ui.WSMessageDeletedMsg{ChannelID: gen, TS: m2})
	tomb := f.msgItem(general, "$m1", bob, deletedText)
	tomb.ThreadTS, tomb.ReplyCount, tomb.ReplyUsers = m1, 1, []string{f.userAlias(alice)}
	f.expect("a root's redaction", f.sync(timeline(general, redact("$x3", "$m1", 10000))),
		ui.NewMessageMsg{ChannelID: gen, Message: tomb, Changed: true})
}

func TestLiveOwnEchoIsSentLikeAnyMessage(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	dm := f.roomAlias(liveDM)
	// The UI drops it by TS (selfSend) when this client sent it; one from
	// another device is shown. It never notifies, and never makes the
	// room unread.
	got := f.sync(timeline(liveDM, ltxt(t, "$own", self, 3000, "mine")))
	f.expect("own message", got,
		ui.NewMessageMsg{ChannelID: dm, Message: f.msgItem(liveDM, "$own", self, "mine")})
	f.noNote("own message")
}

func TestLiveNotifications(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	gen, dm := f.roomAlias(general), f.roomAlias(liveDM)

	f.sync(timeline(liveDM, ltxt(t, "$n1", alice, 3000, "ping")))
	if n := f.note("a DM"); n != "acct: Alice|Alice: ping" {
		t.Fatalf("DM notification = %q", n)
	}
	// A picture with no caption names the file.
	f.sync(timeline(liveDM, lmk(t, "m.room.message", "$img", alice, 3500, nil, map[string]any{
		"msgtype": "m.image", "body": "cat.png", "url": "mxc://example.org/cat",
		"info": map[string]any{"mimetype": "image/png", "w": 10, "h": 10},
	})))
	if n := f.note("a picture"); n != "acct: Alice|Alice: cat.png" {
		t.Fatalf("picture notification = %q", n)
	}
	mention := lmk(t, "m.room.message", "$n2", bob, 4000, nil, map[string]any{
		"msgtype": "m.text", "body": "Me: look", "format": "org.matrix.custom.html",
		"formatted_body": `<a href="https://matrix.to/#/@me:example.org">Me</a>: look`,
	})
	got := f.sync(timeline(general, mention))
	if len(got) != 1 || !strings.Contains(got[0].(ui.NewMessageMsg).Message.Text, "<@"+f.userAlias(self)+">") {
		t.Fatalf("mention = %#v", got)
	}
	if n := f.note("a mention"); n != "acct: #general|Bob: @Me: look" {
		t.Fatalf("mention notification = %q", n)
	}
	// Not for the conversation on screen.
	f.mu.Lock()
	f.active = dm
	f.mu.Unlock()
	f.sync(timeline(liveDM, ltxt(t, "$n3", alice, 5000, "seen")))
	f.noNote("the conversation on screen")
	f.mu.Lock()
	f.active = gen
	f.mu.Unlock()

	// A message still encrypted notifies once it is decrypted.
	enc := lmk(t, "m.room.encrypted", "$enc", alice, 6000, nil, map[string]any{
		"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "AAA", "session_id": "s", "sender_key": "k",
	})
	got = f.sync(timeline(liveDM, enc))
	f.expect("an encrypted message", got,
		ui.NewMessageMsg{ChannelID: dm, Message: f.msgItem(liveDM, "$enc", alice, undecryptableText)})
	f.noNote("an encrypted message")
	dec := lmk(t, "m.room.message", "$enc", alice, 6000, nil, map[string]any{"msgtype": "m.text", "body": "secret"})
	dec.Mautrix.WasEncrypted = true
	got = f.step(session.LateDecrypted{Room: liveDM, Events: []*event.Event{dec}})
	f.expect("the decryption", got, ui.NewMessageMsg{ChannelID: dm, Message: f.msgItem(liveDM, "$enc", alice, "secret"), Changed: true})
	if n := f.note("the decryption"); n != "acct: Alice|Alice: secret" {
		t.Fatalf("decrypted notification = %q", n)
	}
}

func TestLiveReceiptsAndReadState(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	gen := f.roomAlias(general)
	receipt := func(ev id.EventID, thread string) *event.Event {
		r := map[string]any{"ts": 1}
		if thread != "" {
			r["thread_id"] = thread
		}
		return lmk(t, "m.receipt", "", "", 0, nil, map[string]any{
			string(ev): map[string]any{"m.read": map[string]any{string(self): r}},
		})
	}

	// The account's receipt (from any device: the UI tells its own marks
	// apart) is Slack's channel_marked.
	resp := lsync()
	ljoin(resp, general).Ephemeral.Events = []*event.Event{receipt("$g1", "")}
	g1 := f.ts(general, "$g1")
	f.expect("own receipt", f.sync(resp),
		ui.ReadStateChangedMsg{WorkspaceID: f.b.TeamID(), ChannelID: gen},
		ui.ChannelMarkedRemoteMsg{ChannelID: gen, TS: g1, UnreadCount: 0})

	// Another read-state change is a refresh.
	resp = lsync()
	ljoin(resp, general).AccountData.Events = []*event.Event{lmk(t, "m.marked_unread", "", "", 0, nil, map[string]any{"unread": true})}
	f.expect("marked unread", f.sync(resp), ui.ReadStateChangedMsg{WorkspaceID: f.b.TeamID(), ChannelID: gen})
	resp = lsync()
	ljoin(resp, general).AccountData.Events = []*event.Event{lmk(t, "m.marked_unread", "", "", 0, nil, map[string]any{"unread": false})}
	f.sync(resp)

	// A threaded one is thread_marked.
	f.sync(timeline(general, ltxt(t, "$root", bob, 3000, "root")))
	resp = lsync()
	ljoin(resp, general).Ephemeral.Events = []*event.Event{receipt("$root", "$root")}
	f.expect("own thread receipt", f.sync(resp),
		ui.ThreadMarkedRemoteMsg{ChannelID: gen, ThreadTS: f.ts(general, "$root"), LastRead: f.ts(general, "$root")},
		ui.ThreadsListDirtyMsg{TeamID: f.b.TeamID()})
}

func TestLiveTypingIsRefreshedWhileItLasts(t *testing.T) {
	old := typingRefresh
	typingRefresh = 20 * time.Millisecond
	t.Cleanup(func() { typingRefresh = old })
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	gen := f.roomAlias(general)
	typing := func(users ...string) *mautrix.RespSync {
		resp := lsync()
		ljoin(resp, general).Ephemeral.Events = []*event.Event{lmk(t, "m.typing", "", "", 0, nil, map[string]any{"user_ids": users})}
		return resp
	}
	want := ui.UserTypingMsg{ChannelID: gen, UserID: f.userAlias(alice), WorkspaceID: f.b.TeamID()}
	f.expect("typing", f.sync(typing(string(alice), string(self))), want)
	time.Sleep(100 * time.Millisecond)
	f.flush()
	again := f.take()
	if len(again) < 2 || again[0] != want || again[len(again)-1] != want {
		t.Fatalf("while typing: %#v", again)
	}
	// Stopping sends nothing (a refresh may have raced it), and ends the
	// refreshes.
	for _, m := range f.sync(typing()) {
		if m != want {
			t.Fatalf("stopping sent %#v", m)
		}
	}
	time.Sleep(60 * time.Millisecond)
	f.flush()
	if got := f.take(); len(got) != 0 {
		t.Fatalf("after typing stopped: %#v", got)
	}
}

func TestLiveRoomsAndNames(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	const random id.RoomID = "!random:example.org"
	resp := lsync()
	jr := ljoin(resp, random)
	jr.State.Events = []*event.Event{lstate(t, "m.room.name", "", bob, map[string]any{"name": "random"}), lmember(t, self, "Me")}
	got := f.sync(resp)
	snap := f.m.Snapshot()
	ra := f.roomAlias(random)
	var item sidebar.ChannelItem
	var finder core.ChannelFinderItem
	for i, c := range snap.Channels {
		if c.ID == ra {
			item, finder = c, snap.Finder[i]
		}
	}
	f.expect("a room joined", got,
		ui.ConversationOpenedMsg{TeamID: f.b.TeamID(), Item: item, FinderItem: finder},
		ui.ChannelResolvedMsg{TeamID: f.b.TeamID(), ChannelID: ra, Name: "random"},
		ui.SectionsRefreshedMsg{TeamID: f.b.TeamID(), Channels: snap.Channels},
		// Its member events name the account (unchanged; the UI skips
		// it), and it has a read state.
		ui.UserResolvedMsg{TeamID: f.b.TeamID(), UserID: f.userAlias(self), DisplayName: "Me"},
		ui.ReadStateChangedMsg{WorkspaceID: f.b.TeamID(), ChannelID: ra})

	// Leaving drops the row.
	resp = lsync()
	resp.Rooms.Leave[random] = &mautrix.SyncLeftRoom{}
	resp.Rooms.Leave[random].Timeline.Events = []*event.Event{lmk(t, "m.room.member", "$leave", self, 9000, ptr(string(self)), map[string]any{"membership": "leave"})}
	got = f.sync(resp)
	refreshed := slices.IndexFunc(got, func(m tea.Msg) bool {
		r, ok := m.(ui.SectionsRefreshedMsg)
		return ok && len(r.Channels) == 2 && !slices.ContainsFunc(r.Channels, func(c sidebar.ChannelItem) bool { return c.ID == ra })
	})
	if refreshed < 0 || slices.ContainsFunc(got, func(m tea.Msg) bool { _, ok := m.(ui.ConversationOpenedMsg); return ok }) {
		t.Fatalf("after leaving: %#v", got)
	}

	// A new display name.
	resp = lsync()
	ljoin(resp, general).Timeline.Events = []*event.Event{lmember(t, bob, "Robert")}
	f.expect("a rename", f.sync(resp), ui.UserResolvedMsg{TeamID: f.b.TeamID(), UserID: f.userAlias(bob), DisplayName: "Robert"})
}

func ptr(s string) *string { return &s }

func TestBackfillSendsNothing(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	ch, err := f.m.Backfill(context.Background(), general, []*event.Event{ltxt(t, "$old", bob, 500, "old")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(ch.Timeline) == 0 || ch.Timeline[0].Live {
		t.Fatalf("backfill changes = %+v", ch.Timeline)
	}
	var sent []tea.Msg
	r := &liveRun{b: f.b, l: f.b.live, send: func(m tea.Msg) { sent = append(sent, m) }, started: true,
		typing: map[id.RoomID][]id.UserID{}, undecrypted: map[id.EventID]bool{}}
	r.changes(context.Background(), rooms.Changes{Timeline: ch.Timeline}, true, false)
	if len(sent) != 0 {
		t.Fatalf("backfill sent %#v", sent)
	}
	f.noNote("backfill")
}

// TestLiveDrivesTheUI runs the startup and live messages through a real
// App with the backend installed, and reads what it draws.
func TestLiveDrivesTheUI(t *testing.T) {
	f := newLiveFixture(t)
	startup := f.startup(liveWorld(t))
	app := ui.NewApp()
	f.b.Install(app)
	drive(t, app, tea.WindowSizeMsg{Width: 140, Height: 40})
	for _, m := range startup {
		drive(t, app, m)
	}
	screen := ansi.Strip(app.View().Content)
	for _, s := range []string{"general", "Alice", "Rooms", "Direct Messages", "welcome"} {
		if !strings.Contains(screen, s) {
			t.Fatalf("startup screen lacks %q:\n%s", s, screen)
		}
	}
	if app.ActiveChannelID() != f.roomAlias(general) {
		t.Fatalf("active = %q", app.ActiveChannelID())
	}

	for _, m := range f.sync(timeline(general, ltxt(t, "$live", bob, 3000, "live words"))) {
		drive(t, app, m)
	}
	if screen := ansi.Strip(app.View().Content); !strings.Contains(screen, "live words") {
		t.Fatalf("live message not shown:\n%s", screen)
	}

	// The echo of a message this client sent (MessageSentMsg recorded its
	// TS) is not shown twice.
	echo := f.sync(timeline(general, ltxt(t, "$mine", self, 4000, "my own words")))
	sentMsg := echo[0].(ui.NewMessageMsg)
	drive(t, app, ui.MessageSentMsg{ChannelID: sentMsg.ChannelID, Message: sentMsg.Message})
	for _, m := range echo {
		drive(t, app, m)
	}
	if n := strings.Count(ansi.Strip(app.View().Content), "my own words"); n != 1 {
		t.Fatalf("own message shown %d times", n)
	}

	// A room joined live gets its sidebar row.
	resp := lsync()
	ljoin(resp, "!random:example.org").State.Events = []*event.Event{
		lstate(t, "m.room.name", "", bob, map[string]any{"name": "randomroom"}), lmember(t, self, "Me"),
	}
	for _, m := range f.sync(resp) {
		drive(t, app, m)
	}
	if screen := ansi.Strip(app.View().Content); !strings.Contains(screen, "randomroom") {
		t.Fatalf("joined room not in the sidebar:\n%s", screen)
	}
}

// drive delivers msg to app and, a few levels deep, the messages its
// commands produce within a short wait (ticks and slow work are dropped).
func drive(t *testing.T, app *ui.App, msg tea.Msg) {
	t.Helper()
	var run func(msg tea.Msg, depth int)
	run = func(msg tea.Msg, depth int) {
		if batch, ok := msg.(tea.BatchMsg); ok {
			for _, c := range batch {
				run(cmdMsg(c), depth)
			}
			return
		}
		if msg == nil {
			return
		}
		_, cmd := app.Update(msg)
		if depth < 4 {
			run(cmdMsg(cmd), depth+1)
		}
	}
	run(msg, 0)
}

// cmdMsg runs cmd and returns its message, nil if it has none within
// 200ms.
func cmdMsg(cmd tea.Cmd) tea.Msg {
	if cmd == nil {
		return nil
	}
	ch := make(chan tea.Msg, 1)
	go func() { ch <- cmd() }()
	select {
	case m := <-ch:
		return m
	case <-time.After(200 * time.Millisecond):
		return nil
	}
}
