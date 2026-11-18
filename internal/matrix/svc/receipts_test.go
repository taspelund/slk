package svc

import (
	"context"
	"reflect"
	"testing"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ui"
)

const dave id.UserID = "@dave:example.org"

// readReceipt applies user's public receipt on ev (thread "" for an
// unthreaded one) through /sync, read at ms (unix milliseconds).
func (h *harness) readReceipt(user id.UserID, ev id.EventID, thread string, ms int64) {
	h.t.Helper()
	r := map[string]any{"ts": ms}
	if thread != "" {
		r["thread_id"] = thread
	}
	resp := lsync()
	ljoin(resp, general).Ephemeral.Events = []*event.Event{lmk(h.t, "m.receipt", "", "", 0, nil, map[string]any{
		string(ev): map[string]any{"m.read": map[string]any{string(user): r}},
	})}
	h.apply(resp)
}

// reply posts an m.text reply in root's thread live.
func (h *harness) reply(sender id.UserID, root id.EventID, body string) (*event.Event, string) {
	e := h.ev(sender, "m.room.message", map[string]any{"msgtype": "m.text", "body": body,
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}})
	return e, h.live(e)
}

// seen is each item's text and markers (as user IDs), for comparing.
func (h *harness) seen(items []core.MessageItem) map[string][]id.UserID {
	h.t.Helper()
	out := map[string][]id.UserID{}
	for _, it := range items {
		for _, a := range it.SeenBy {
			u, ok := h.res().MXID(a)
			if !ok {
				h.t.Fatalf("unknown user alias %q", a)
			}
			out[it.Text] = append(out[it.Text], u)
		}
	}
	return out
}

func (h *harness) channel() []core.MessageItem {
	h.t.Helper()
	return h.b.fetch(h.alias, nil).(ui.MessagesLoadedMsg).Messages
}

func (h *harness) thread(rootTS string) []core.MessageItem {
	h.t.Helper()
	return h.b.fetchThread(h.alias, rootTS).(ui.ThreadRepliesLoadedMsg).Replies
}

func assertSeen(t *testing.T, what string, got, want map[string][]id.UserID) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s: markers = %v, want %v", what, got, want)
	}
}

// TestMarkersSitUnderTheNewestMessageEachUserRead: a marker per other
// user, never the account's own or the sender's, newest reader first; a
// user moving forward leaves the old message, and a receipt that would
// move one back is ignored.
func TestMarkersSitUnderTheNewestMessageEachUserRead(t *testing.T) {
	h := newHarness(t, nil)
	one, _ := h.text(alice, "one")
	two, _ := h.text(bob, "two")
	three, _ := h.text(alice, "three")

	h.readReceipt(bob, one.ID, "", 1000)
	h.readReceipt(carol, two.ID, "main", 1000)
	h.readReceipt(alice, three.ID, "", 1000) // her own message: no marker
	h.receipt(three.ID, "")                  // the account's: no marker
	assertSeen(t, "first", h.seen(h.channel()), map[string][]id.UserID{"one": {bob}, "two": {carol}})

	h.readReceipt(dave, two.ID, "", 1000)
	h.readReceipt(bob, three.ID, "", 2000)
	assertSeen(t, "bob moved", h.seen(h.channel()), map[string][]id.UserID{"two": {carol, dave}, "three": {bob}})

	// Older: kept where he was.
	h.readReceipt(bob, one.ID, "", 3000)
	assertSeen(t, "bob's older receipt", h.seen(h.channel()), map[string][]id.UserID{"two": {carol, dave}, "three": {bob}})

	// Newest reader first: bob read four after carol.
	four, _ := h.text(alice, "four")
	h.readReceipt(carol, four.ID, "", 4000)
	h.readReceipt(bob, four.ID, "", 5000)
	want := map[string][]id.UserID{"two": {dave}, "four": {bob, carol}}
	assertSeen(t, "four", h.seen(h.channel()), want)
	assertSeen(t, "ReadCache", h.seen(h.b.readCache(h.alias)), want)
}

// TestAReceiptOnAHiddenEventCountsAtTheNewestShownMessage: reactions,
// edits and redacted messages are not rows; a receipt on one puts the
// marker under the newest shown message at or before it.
func TestAReceiptOnAHiddenEventCountsAtTheNewestShownMessage(t *testing.T) {
	h := newHarness(t, nil)
	one, oneTS := h.text(alice, "one")
	react := h.ev(alice, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": one.ID, "key": "👍"}})
	h.live(react)
	edit := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "* one!",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "one!"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": one.ID}})
	h.live(edit)
	gone, _ := h.text(alice, "gone")
	h.redact(gone.ID)

	h.readReceipt(bob, react.ID, "", 1000)
	h.readReceipt(carol, edit.ID, "", 1000)
	h.readReceipt(dave, gone.ID, "", 1000)
	// Furthest read first.
	assertSeen(t, "hidden events", h.seen(h.channel()), map[string][]id.UserID{"one!": {dave, carol, bob}})

	// Live: dave's marker is under one.
	if ts, err := h.b.markerAt(context.Background(), general, "", dave); err != nil || ts != oneTS {
		t.Errorf("markerAt = %q, %v; want %q", ts, err, oneTS)
	}
}

// TestThreadedReceipts: an unthreaded receipt reads the main timeline and
// threads, "main" the main timeline only, a thread's the thread only.
func TestThreadedReceipts(t *testing.T) {
	h := newHarness(t, nil)
	root, rootTS := h.text(alice, "root")
	r1, _ := h.reply(alice, root.ID, "r1")
	r2, _ := h.reply(alice, root.ID, "r2")
	later, _ := h.text(alice, "later")

	h.readReceipt(bob, r1.ID, "", 1000)                  // unthreaded on a reply
	h.readReceipt(carol, later.ID, "main", 1000)         // main only
	h.readReceipt(dave, r2.ID, string(root.ID), 1000)    // the thread only
	h.readReceipt(carol, root.ID, string(root.ID), 1000) // carol opened the thread too

	assertSeen(t, "channel", h.seen(h.channel()), map[string][]id.UserID{"root": {bob}, "later": {carol}})
	// carol's thread receipt is on the root, which is no reply: no
	// marker in the thread.
	assertSeen(t, "thread", h.seen(h.thread(rootTS)), map[string][]id.UserID{"r1": {bob}, "r2": {dave}})

	// An unthreaded receipt past the thread reads all of it.
	h.readReceipt(bob, later.ID, "", 2000)
	assertSeen(t, "thread after bob read on", h.seen(h.thread(rootTS)), map[string][]id.UserID{"r2": {bob, dave}})
	assertSeen(t, "thread cache", h.seen(h.b.threadCache(h.alias, rootTS)), map[string][]id.UserID{"r2": {bob, dave}})
}

// TestAReceiptStoredBeforeItsEventResolvesLater: a receipt on an event
// not mapped yet is stored without a TS, and counts once the event is.
func TestAReceiptStoredBeforeItsEventResolvesLater(t *testing.T) {
	h := newHarness(t, nil)
	h.text(alice, "one")
	two := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "two"})
	h.readReceipt(bob, two.ID, "", 1000)
	if rec, ok, err := h.st.GetReceipt(context.Background(), general, "", bob); err != nil || !ok || rec.TS != "" || rec.ReadAt != 1000 {
		t.Fatalf("stored receipt = %+v %v %v, want one without a TS", rec, ok, err)
	}
	assertSeen(t, "before the event", h.seen(h.channel()), map[string][]id.UserID{})
	h.live(two)
	assertSeen(t, "after", h.seen(h.channel()), map[string][]id.UserID{"two": {bob}})
}

// TestOlderPagesLeaveMarkersOfLaterMessagesOut: a page fetched before the
// messages shown holds only the markers that sit in it.
func TestOlderPagesLeaveMarkersOfLaterMessagesOut(t *testing.T) {
	h := newHarness(t, nil)
	one, _ := h.text(alice, "one")
	h.text(alice, "two")
	three, threeTS := h.text(alice, "three")
	h.readReceipt(bob, three.ID, "", 1000)
	h.readReceipt(carol, one.ID, "", 1000)
	older := h.b.fetchOlder(h.alias, threeTS).(ui.OlderMessagesLoadedMsg).Messages
	assertSeen(t, "older page", h.seen(older), map[string][]id.UserID{"one": {carol}})

	around := h.b.fetchAround(h.alias, threeTS).(ui.MessagesAroundLoadedMsg).Messages
	assertSeen(t, "around", h.seen(around), map[string][]id.UserID{"one": {carol}, "three": {bob}})
}

// TestSeenByListsEveryoneAtOrAfterTheMessage is :seen's port.
func TestSeenByListsEveryoneAtOrAfterTheMessage(t *testing.T) {
	h := newHarness(t, nil)
	h.b.tsFormat = "15:04"
	one, oneTS := h.text(alice, "one")
	two, twoTS := h.text(bob, "two")
	root, rootTS := h.text(alice, "root")
	reply, replyTS := h.reply(alice, root.ID, "reply")

	at := time.Date(2026, 11, 2, 9, 30, 0, 0, time.Local)
	h.readReceipt(bob, two.ID, "", at.UnixMilli())
	h.readReceipt(carol, one.ID, "", at.Add(time.Hour).UnixMilli())
	h.readReceipt(alice, two.ID, "", 0) // the server gave no time
	h.receipt(two.ID, "")
	h.readReceipt(dave, reply.ID, string(root.ID), at.UnixMilli())

	svc := receiptService{h.b}
	names := func(rs []core.Reader, err error) []string {
		t.Helper()
		if err != nil {
			t.Fatalf("SeenBy: %v", err)
		}
		out := []string{}
		for _, r := range rs {
			out = append(out, r.Name+"|"+r.ReadTime)
		}
		return out
	}
	// one: alice sent it; carol read latest, then bob; never the account.
	if got := names(svc.SeenBy(h.alias, "", oneTS)); !reflect.DeepEqual(got, []string{"carol|10:30", "Bob|09:30"}) {
		t.Errorf("SeenBy(one) = %q", got)
	}
	// two: bob sent it; alice read it, time unknown.
	if got := names(svc.SeenBy(h.alias, "", twoTS)); !reflect.DeepEqual(got, []string{"Alice Liddell|"}) {
		t.Errorf("SeenBy(two) = %q", got)
	}
	// The reply: dave's thread receipt; the root itself is the channel's.
	if got := names(svc.SeenBy(h.alias, rootTS, replyTS)); !reflect.DeepEqual(got, []string{"dave|09:30"}) {
		t.Errorf("SeenBy(reply) = %q", got)
	}
	if got := names(svc.SeenBy(h.alias, "", rootTS)); len(got) != 0 {
		t.Errorf("SeenBy(root) = %q", got)
	}
	rs, _ := svc.SeenBy(h.alias, "", oneTS)
	if rs[1].ReadTS == "" || rs[1].UserID != h.res().UserAlias(bob) {
		t.Errorf("reader = %+v", rs[1])
	}
}

// TestLiveReceiptsMoveMarkers: other users' receipts in a room on screen
// send ReadMarkersMovedMsg per timeline they read.
func TestLiveReceiptsMoveMarkers(t *testing.T) {
	f := newLiveFixture(t)
	f.startup(liveWorld(t))
	gen := f.roomAlias(general)
	receipt := func(user id.UserID, ev id.EventID, thread string) *mautrix.RespSync {
		r := map[string]any{"ts": 1}
		if thread != "" {
			r["thread_id"] = thread
		}
		resp := lsync()
		ljoin(resp, general).Ephemeral.Events = []*event.Event{lmk(t, "m.receipt", "", "", 0, nil, map[string]any{
			string(ev): map[string]any{"m.read": map[string]any{string(user): r}},
		})}
		return resp
	}
	g1 := f.ts(general, "$g1")
	f.expect("bob's receipt", f.sync(receipt(bob, "$g1", "")),
		ui.ReadMarkersMovedMsg{ChannelID: gen, Moves: []ui.SeenMove{{UserID: f.userAlias(bob), TS: g1}}})
	// On her own message: her marker goes.
	f.expect("alice's receipt", f.sync(receipt(alice, "$g1", "")),
		ui.ReadMarkersMovedMsg{ChannelID: gen, Moves: []ui.SeenMove{{UserID: f.userAlias(alice), TS: ""}}})
	// Again on the same event: nothing moved.
	f.expect("bob again", f.sync(receipt(bob, "$g1", "")))

	f.sync(timeline(general,
		ltxt(t, "$root", alice, 3000, "root"),
		lmk(t, "m.room.message", "$r1", alice, 4000, nil, map[string]any{"msgtype": "m.text", "body": "r1",
			"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": "$root"}})))
	root, r1 := f.ts(general, "$root"), f.ts(general, "$r1")
	f.expect("carol in the thread", f.sync(receipt(carol, "$r1", "$root")),
		ui.ReadMarkersMovedMsg{ChannelID: gen, ThreadTS: root, Moves: []ui.SeenMove{{UserID: f.userAlias(carol), TS: r1}}})

	// Unthreaded: the channel, and the thread the UI has open.
	f.b.threadOpened(general, "$root")
	f.expect("bob unthreaded", f.sync(receipt(bob, "$r1", "")),
		ui.ReadMarkersMovedMsg{ChannelID: gen, Moves: []ui.SeenMove{{UserID: f.userAlias(bob), TS: root}}},
		ui.ReadMarkersMovedMsg{ChannelID: gen, ThreadTS: root, Moves: []ui.SeenMove{{UserID: f.userAlias(bob), TS: r1}}})
}
