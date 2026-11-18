package svc

import (
	"context"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/ui"
)

// texts returns the items' texts.
func texts(items []core.MessageItem) []string {
	out := []string{}
	for _, it := range items {
		out = append(out, it.Text)
	}
	return out
}

func assertAscending(t *testing.T, items []core.MessageItem) {
	t.Helper()
	for i := 1; i < len(items); i++ {
		if items[i-1].TS >= items[i].TS {
			t.Fatalf("items out of order at %d: %s >= %s", i, items[i-1].TS, items[i].TS)
		}
	}
}

func TestFetchOnAnEmptyStorePagesTheServer(t *testing.T) {
	var h *harness
	var older []*event.Event // newest first, as /messages returns them
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/rooms/"+string(general)+"/messages") {
			http.NotFound(w, r)
			return
		}
		// One page, then the start of the room (no end token).
		writeJSON(t, w, map[string]any{"start": "s0", "chunk": wires(older...)})
	})
	first := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "one"})
	second := h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "two"})
	react := h.ev(alice, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": second.ID, "key": "👍"}})
	third := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "three"})
	older = []*event.Event{third, react, second, first}

	var marked []string
	var mu sync.Mutex
	done := make(chan struct{})
	mark := func(ch ids.ChannelID, ts ids.MessageTS) core.Msg {
		mu.Lock()
		marked = append(marked, string(ch)+" "+string(ts))
		mu.Unlock()
		close(done)
		return nil
	}
	msg, ok := h.b.fetch(h.alias, mark).(ui.MessagesLoadedMsg)
	if !ok {
		t.Fatalf("Fetch returned %T", msg)
	}
	if got := texts(msg.Messages); !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Fatalf("messages = %q", got)
	}
	assertAscending(t, msg.Messages)
	if r := msg.Messages[1].Reactions; len(r) != 1 || r[0].Emoji != "+1" {
		t.Errorf("reactions on two = %+v", r)
	}
	if reqs := h.reqs(); len(reqs) != 1 || !strings.Contains(reqs[0], "dir=b") || strings.Contains(reqs[0], "from=") {
		t.Errorf("requests = %q, want one /messages from the live end", reqs)
	}
	if got := h.f.backfilled; len(got) != 1 || !reflect.DeepEqual(got[0], []id.EventID{third.ID, react.ID, second.ID, first.ID}) {
		t.Errorf("Backfill got %v, want newest first", got)
	}
	<-done
	newest := msg.Messages[2].TS
	if msg.MarkedTS != newest || !reflect.DeepEqual(marked, []string{h.alias + " " + newest}) {
		t.Errorf("MarkedTS = %q, marks = %q, want %q", msg.MarkedTS, marked, newest)
	}
	if h.b.syncedAt(h.alias) == 0 {
		t.Errorf("SyncedAt not set after Fetch")
	}

	// The start was reached: fetching again asks the server nothing.
	again := h.b.fetch(h.alias, nil).(ui.MessagesLoadedMsg)
	if len(again.Messages) != 3 || again.MarkedTS != "" || len(h.reqs()) != 1 {
		t.Errorf("second fetch: %d messages, MarkedTS %q, %d requests", len(again.Messages), again.MarkedTS, len(h.reqs()))
	}
	if got := texts(h.b.readCache(h.alias)); !reflect.DeepEqual(got, []string{"one", "two", "three"}) {
		t.Errorf("ReadCache = %q", got)
	}
}

func TestFetchUsesTheLastReadTSFromTheSnapshot(t *testing.T) {
	h := newHarness(t, nil)
	_, ts := h.text(alice, "hi")
	h.f.readStates[h.alias] = core.ReadState{LastReadTS: ts}
	msg := h.b.fetch(h.alias, nil).(ui.MessagesLoadedMsg)
	if msg.LastReadTS != ts || len(msg.Messages) != 1 {
		t.Fatalf("Fetch = %+v", msg)
	}
	if msg := h.b.fetch("CNOSUCHROOM", nil).(ui.MessagesLoadedMsg); msg.Messages != nil {
		t.Errorf("unknown room: Messages = %v, want nil (keep what is shown)", msg.Messages)
	}
}

func TestFetchOlderPagesTheStoreThenTheServer(t *testing.T) {
	var h *harness
	var server []*event.Event // older than everything stored, newest first
	var oldestStored *event.Event
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/context/"):
			// Nothing before the oldest stored event in this reply; the
			// token leads to the server's history.
			writeJSON(t, w, map[string]any{"start": "ctx-start", "end": "ctx-end", "event": wire(oldestStored)})
		case strings.HasSuffix(r.URL.Path, "/messages"):
			switch r.URL.Query().Get("from") {
			case "ctx-start":
				writeJSON(t, w, map[string]any{"start": "ctx-start", "end": "p2", "chunk": wires(server[:2]...)})
			case "p2":
				writeJSON(t, w, map[string]any{"start": "p2", "end": "p3", "chunk": wires(server[2:]...)})
			default:
				writeJSON(t, w, map[string]any{"start": "p3", "chunk": []any{}})
			}
		default:
			http.NotFound(w, r)
		}
	})
	// Server history first (older times), then 120 stored messages.
	for i := range 3 {
		server = append([]*event.Event{h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "old" + string(rune('A'+i))})}, server...)
	}
	var stored []string
	for i := range 120 {
		e, ts := h.text(alice, "m"+itoa3(i))
		if i == 0 {
			oldestStored = e
		}
		stored = append(stored, ts)
	}

	// Within the store: the 50 before the anchor, no requests.
	anchor := stored[70]
	msg := h.b.fetchOlder(h.alias, anchor).(ui.OlderMessagesLoadedMsg)
	if msg.AnchorTS != anchor || len(msg.Messages) != pageSize {
		t.Fatalf("FetchOlder = anchor %q, %d messages", msg.AnchorTS, len(msg.Messages))
	}
	assertAscending(t, msg.Messages)
	if msg.Messages[0].TS != stored[20] || msg.Messages[pageSize-1].TS != stored[69] {
		t.Errorf("page = %s..%s, want %s..%s", msg.Messages[0].TS, msg.Messages[pageSize-1].TS, stored[20], stored[69])
	}
	if n := len(h.reqs()); n != 0 {
		t.Errorf("%d requests for a page the store holds", n)
	}

	// Past the oldest stored: the rest of the store, then the server.
	msg = h.b.fetchOlder(h.alias, stored[10]).(ui.OlderMessagesLoadedMsg)
	assertAscending(t, msg.Messages)
	want := []string{"oldA", "oldB", "oldC"}
	for i := range 10 {
		want = append(want, "m"+itoa3(i))
	}
	if got := texts(msg.Messages); !reflect.DeepEqual(got, want) {
		t.Fatalf("older = %q, want %q", got, want)
	}
	for _, it := range msg.Messages {
		if it.TS >= stored[10] {
			t.Errorf("%s is not older than the anchor %s", it.TS, stored[10])
		}
	}
	reqs := h.reqs()
	if len(reqs) != 4 || !strings.Contains(reqs[0], "/context/") || !strings.Contains(reqs[1], "from=ctx-start") ||
		!strings.Contains(reqs[2], "from=p2") || !strings.Contains(reqs[3], "from=p3") {
		t.Errorf("requests = %q", reqs)
	}

	// The start is known now: older than the oldest is empty, with no
	// more requests.
	oldest := msg.Messages[0].TS
	msg = h.b.fetchOlder(h.alias, oldest).(ui.OlderMessagesLoadedMsg)
	if len(msg.Messages) != 0 || msg.Messages == nil || len(h.reqs()) != 4 {
		t.Errorf("before the start: %d messages (nil %v), %d requests", len(msg.Messages), msg.Messages == nil, len(h.reqs()))
	}
}

func TestPagingBackStartsFromAGapAtTheOldestEvent(t *testing.T) {
	var h *harness
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"start": "x", "chunk": []any{}})
	})
	_, ts := h.text(alice, "first after a limited sync")
	if _, err := h.st.AddGap(context.Background(), general, "gap-token", ts); err != nil {
		t.Fatal(err)
	}
	h.b.fetch(h.alias, nil)
	if reqs := h.reqs(); len(reqs) != 1 || !strings.Contains(reqs[0], "/messages") || !strings.Contains(reqs[0], "from=gap-token") {
		t.Fatalf("requests = %q, want /messages from the gap's prev_batch", reqs)
	}
}

func TestFetchAroundReturnsAPageAroundTheTarget(t *testing.T) {
	h := newHarness(t, nil)
	var stored []string
	for i := range 100 {
		_, ts := h.text(alice, "m"+itoa3(i))
		stored = append(stored, ts)
	}
	msg := h.b.fetchAround(h.alias, stored[60]).(ui.MessagesAroundLoadedMsg)
	if msg.Err != nil || msg.TargetTS != stored[60] {
		t.Fatalf("FetchAround = %+v", msg)
	}
	assertAscending(t, msg.Messages)
	if _, ok := byTS(msg.Messages, stored[60]); !ok {
		t.Fatalf("target not in the window")
	}
	if first, last := msg.Messages[0].TS, msg.Messages[len(msg.Messages)-1].TS; first != stored[35] || last != stored[84] {
		t.Errorf("window = %s..%s, want %s..%s", first, last, stored[35], stored[84])
	}
	if msg := h.b.fetchAround(h.alias, "0000000001.000000").(ui.MessagesAroundLoadedMsg); msg.Err == nil {
		t.Errorf("an unknown TS gave no error")
	}
}

func TestThreadFetchReadsRepliesThroughTheServer(t *testing.T) {
	var h *harness
	var replies []*event.Event // newest first
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/relations/") || !strings.HasSuffix(r.URL.Path, "/m.thread") {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]any{"chunk": wires(replies...)})
	})
	root, rootTS := h.text(alice, "root")
	reply := func(sender id.UserID, body string) *event.Event {
		return h.ev(sender, "m.room.message", map[string]any{"msgtype": "m.text", "body": body, "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}})
	}
	r1 := reply(bob, "r1")
	h.live(r1)
	r2 := reply(self, "r2")
	h.live(r2)
	// r3 is newer than the stored timeline's start but was never stored
	// (a gap): Backfill could only give it a TS before the root, so it
	// is left out.
	r3 := reply(bob, "r3")
	replies = []*event.Event{r3, r2, r1}

	msg := h.b.fetchThread(h.alias, rootTS).(ui.ThreadRepliesLoadedMsg)
	if msg.ThreadTS != rootTS || msg.FromCache || !reflect.DeepEqual(texts(msg.Replies), []string{"r1", "r2"}) {
		t.Fatalf("Fetch = %+v", msg)
	}
	assertAscending(t, msg.Replies)
	for _, r := range msg.Replies {
		if r.ThreadTS != rootTS || r.TS <= rootTS {
			t.Errorf("reply %q: TS %s, ThreadTS %q, want after root %s", r.Text, r.TS, r.ThreadTS, rootTS)
		}
	}
	if got := h.f.backfilled; len(got) != 1 || !reflect.DeepEqual(got[0], []id.EventID{r2.ID, r1.ID}) {
		t.Errorf("Backfill got %v, want the mapped replies newest first", got)
	}
	if _, mapped, _ := h.f.ids.TS(context.Background(), general, r3.ID); mapped {
		t.Errorf("r3 got a TS")
	}
	cache := h.b.threadCache(h.alias, rootTS)
	if !reflect.DeepEqual(texts(cache), []string{"root", "r1", "r2"}) || cache[0].ReplyCount != 2 || cache[0].ThreadTS != rootTS {
		t.Errorf("CacheRead = %+v", cache)
	}
	if reqs := h.reqs(); len(reqs) != 1 || !strings.Contains(reqs[0], "dir=b") {
		t.Errorf("requests = %q", reqs)
	}

	h.f.threadReceipts[root.ID] = rooms.Receipt{Room: general, Thread: root.ID, TS: msg.Replies[0].TS}
	if got := h.b.threadLastRead(h.alias, rootTS); got != msg.Replies[0].TS {
		t.Errorf("ThreadLastRead = %q, want %q", got, msg.Replies[0].TS)
	}
}

func TestFetchAroundAnEventNotStoredUsesContext(t *testing.T) {
	var h *harness
	var before, after []*event.Event
	var target *event.Event
	h = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "/context/") {
			http.NotFound(w, r)
			return
		}
		writeJSON(t, w, map[string]any{
			"start": "s", "end": "e", "event": wire(target),
			"events_before": wires(before...), "events_after": wires(after...),
		})
	})
	old := h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "older"})
	before = []*event.Event{old}
	target = h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "target"})
	_, firstTS := h.text(alice, "first stored")
	h.text(alice, "second stored")
	// The target has a TS (it was stored once, then deleted) and is
	// older than everything stored; the event after it in the reply is
	// newer and has none, so it cannot be placed.
	targetTS, err := h.f.ids.TSForBackfill(context.Background(), general, target.ID, time.UnixMilli(target.Timestamp))
	if err != nil {
		t.Fatal(err)
	}
	unplaced := h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "unplaced"})
	after = []*event.Event{unplaced}

	msg := h.b.fetchAround(h.alias, targetTS).(ui.MessagesAroundLoadedMsg)
	if msg.Err != nil {
		t.Fatalf("FetchAround: %v", msg.Err)
	}
	if got := texts(msg.Messages); !reflect.DeepEqual(got, []string{"older", "target", "first stored", "second stored"}) {
		t.Errorf("window = %q", got)
	}
	assertAscending(t, msg.Messages)
	if it, ok := byTS(msg.Messages, firstTS); !ok || it.Text != "first stored" {
		t.Errorf("stored message lost its TS")
	}
}

func TestThreadFetchFailureKeepsTheCachedView(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"errcode":"M_UNKNOWN","error":"boom"}`, http.StatusInternalServerError)
	})
	_, rootTS := h.text(alice, "root")
	if msg := h.b.fetchThread(h.alias, rootTS).(ui.ThreadRepliesLoadedMsg); msg.Replies != nil {
		t.Fatalf("Replies = %v, want nil on failure", msg.Replies)
	}
}

func TestListThreadsListsThreadsTheAccountIsIn(t *testing.T) {
	h := newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		writeJSON(t, w, map[string]any{"chunk": []any{}})
	})
	mine, mineTS := h.text(self, "my thread")
	theirs, _ := h.text(alice, "not mine")
	joined, joinedTS := h.text(bob, "joined in")
	thread := func(sender id.UserID, root id.EventID, body string) string {
		return h.live(h.ev(sender, "m.room.message", map[string]any{"msgtype": "m.text", "body": body, "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root}}))
	}
	thread(alice, mine.ID, "a")
	lastMine := thread(bob, mine.ID, "b")
	thread(bob, theirs.ID, "c")
	thread(self, joined.ID, "d")
	h.f.threadReceipts[mine.ID] = rooms.Receipt{TS: "0000000000.000000"}

	msg := h.b.listThreads(h.b.TeamID()).(ui.ThreadsListLoadedMsg)
	if msg.TeamID != h.b.TeamID() || !msg.SubscriptionsAvailable || len(msg.Summaries) != 2 {
		t.Fatalf("ListFetch = %+v", msg)
	}
	res := h.f.Resolver()
	got := msg.Summaries[0] // "joined in" has the newest reply
	if got.ThreadTS != joinedTS || got.ParentUserID != res.UserAlias(bob) || got.LastReplyBy != res.UserAlias(self) || got.Unread {
		t.Errorf("first summary = %+v", got)
	}
	got = msg.Summaries[1]
	if got.ThreadTS != mineTS || got.ChannelID != h.alias || got.ParentText != "my thread" || got.ReplyCount != 2 ||
		got.LastReplyTS != lastMine || got.LastReplyBy != res.UserAlias(bob) || !got.Unread || got.ChannelType != "channel" {
		t.Errorf("second summary = %+v", got)
	}
	if reqs := h.reqs(); len(reqs) != 1 || !strings.Contains(reqs[0], "/threads") || !strings.Contains(reqs[0], "include=participated") {
		t.Errorf("requests = %q", reqs)
	}
	h.b.listThreads(h.b.TeamID())
	if n := len(h.reqs()); n != 1 {
		t.Errorf("a second ListFetch made %d requests in all, want the sweep throttled", n)
	}
}

func TestSearchChannel(t *testing.T) {
	h := newHarness(t, nil)
	_, helloTS := h.text(alice, "hello world")
	bye, byeTS := h.text(bob, "goodbye")
	h.text(alice, "unrelated")
	h.live(h.ev(bob, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "* goodbye, hello",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "goodbye, hello"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": bye.ID},
	}))
	root, _ := h.text(alice, "root")
	h.live(h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hello in a thread", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}}))

	msg := h.b.searchChannel(h.alias, "Héllo").(ui.ChannelSearchResultsMsg)
	if msg.Err != nil || msg.ChannelID != h.alias || msg.Query != "Héllo" || !reflect.DeepEqual(msg.Terms, []string{"hello"}) {
		t.Fatalf("search = %+v", msg)
	}
	if want := []string{byeTS, helloTS}; !reflect.DeepEqual(msg.TSes, want) {
		t.Errorf("TSes = %q, want %q", msg.TSes, want)
	}
	if msg := h.b.searchChannel(h.alias, "nothing").(ui.ChannelSearchResultsMsg); len(msg.TSes) != 0 {
		t.Errorf("TSes = %q for no match", msg.TSes)
	}
}

func TestUnreadStatesComeFromTheSnapshot(t *testing.T) {
	h := newHarness(t, nil)
	if got := h.b.unreadWorkspaces(); got != nil {
		t.Errorf("UnreadWorkspaces = %v with nothing unread", got)
	}
	h.f.readStates[h.alias] = core.ReadState{LastReadTS: "1.0", HasUnread: true, MentionCount: 2}
	states := h.b.readStates()
	if !reflect.DeepEqual(states, map[string]core.ReadState{h.alias: {LastReadTS: "1.0", HasUnread: true, MentionCount: 2}}) {
		t.Errorf("ChannelReadStates = %+v", states)
	}
	if got := h.b.unreadWorkspaces(); !reflect.DeepEqual(got, []string{h.b.TeamID()}) {
		t.Errorf("UnreadWorkspaces = %v", got)
	}
}

func TestLookupAndAvatar(t *testing.T) {
	h := newHarness(t, nil)
	var p ports
	h.b.fillRead(&p)
	name, kind, ok := p.channels.Lookup(ids.ChannelID(h.alias))
	if !ok || name != "general" || kind != "channel" {
		t.Errorf("Lookup = %q %q %v", name, kind, ok)
	}
	if _, _, ok := p.channels.Lookup("CNOSUCHROOM"); ok {
		t.Errorf("Lookup of an unknown alias succeeded")
	}
	av := p.avatars.Avatar(h.f.Resolver().UserAlias(alice))
	if !strings.Contains(av, " AL ") || strings.Count(av, "\n") != 1 {
		t.Errorf("Avatar = %q", av)
	}
	if av := p.avatars.Avatar("UNOBODY0000"); av != "" {
		t.Errorf("Avatar of an unknown alias = %q", av)
	}
	if p.workspace.Switch(h.b.TeamID()) != nil {
		t.Errorf("Switch returned a message")
	}
}
