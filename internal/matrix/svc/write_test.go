package svc

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/ui"
)

// writeReq is one request the fake homeserver got: method, unescaped
// path, and JSON body (nil for none).
type writeReq struct {
	method, path string
	body         map[string]any
}

// writeServer is a harness whose homeserver answers writes: sends get
// event IDs from next (in order, then "$sentN"), everything else {}
// unless answer handles it.
type writeServer struct {
	*harness
	mu     sync.Mutex
	got    []writeReq
	next   []id.EventID
	n      int
	answer func(w http.ResponseWriter, r writeReq) bool
	ports  ports
}

func newWriteServer(t *testing.T) *writeServer {
	s := &writeServer{}
	s.harness = newHarness(t, func(w http.ResponseWriter, r *http.Request) {
		path, _ := url.PathUnescape(r.URL.EscapedPath())
		req := writeReq{method: r.Method, path: path}
		if raw, _ := io.ReadAll(r.Body); len(raw) > 0 {
			_ = json.Unmarshal(raw, &req.body)
		}
		s.mu.Lock()
		s.got = append(s.got, req)
		answer := s.answer
		s.mu.Unlock()
		if answer != nil && answer(w, req) {
			return
		}
		if strings.Contains(path, "/send/") || strings.Contains(path, "/redact/") {
			s.mu.Lock()
			var ev id.EventID
			if len(s.next) > 0 {
				ev, s.next = s.next[0], s.next[1:]
			} else {
				s.n++
				ev = id.EventID("$sent" + itoa3(s.n))
			}
			s.mu.Unlock()
			writeJSON(t, w, map[string]any{"event_id": ev})
			return
		}
		writeJSON(t, w, map[string]any{})
	})
	s.b.fillWrite(&s.ports)
	return s
}

// reqs returns the requests so far, and forgets them.
func (s *writeServer) take() []writeReq {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.got
	s.got = nil
	return out
}

// one returns the only request so far whose path contains part.
func (s *writeServer) one(part string) writeReq {
	s.t.Helper()
	var match []writeReq
	for _, r := range s.take() {
		if strings.Contains(r.path, part) {
			match = append(match, r)
		}
	}
	if len(match) != 1 {
		s.t.Fatalf("requests to %q: %+v", part, match)
	}
	return match[0]
}

// relates is a request body's m.relates_to.
func relates(r writeReq) map[string]any {
	m, _ := r.body["m.relates_to"].(map[string]any)
	return m
}

func TestSendGivesTheSentEventTheTSItsEchoGets(t *testing.T) {
	s := newWriteServer(t)
	s.next = []id.EventID{"$mine"}
	msg, ok := s.ports.messages.Send(ids.ChannelID(s.alias), "**hi** <@"+s.res().UserAlias(alice)+">").(ui.MessageSentMsg)
	if !ok {
		t.Fatalf("Send = %#v", msg)
	}
	req := s.one("/send/m.room.message/")
	if req.method != http.MethodPut || !strings.Contains(req.path, "/rooms/"+string(general)+"/") {
		t.Fatalf("request = %+v", req)
	}
	if req.body["format"] != "org.matrix.custom.html" || !strings.Contains(req.body["formatted_body"].(string), "<strong>hi</strong>") {
		t.Errorf("body = %v", req.body)
	}
	if m, _ := req.body["m.mentions"].(map[string]any); !reflect.DeepEqual(m["user_ids"], []any{string(alice)}) {
		t.Errorf("m.mentions = %v", req.body["m.mentions"])
	}
	it := msg.Message
	if it.TS == "" || it.UserID != s.res().UserAlias(self) || it.UserName != "Me" || it.Text != "*hi* <@"+s.res().UserAlias(alice)+">" {
		t.Fatalf("message = %+v", it)
	}
	// The echo maps to the same TS.
	echo := s.ev(self, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hi"})
	echo.ID = "$mine"
	if ts := s.live(echo); ts != it.TS {
		t.Fatalf("echo TS %q, sent TS %q", ts, it.TS)
	}
}

func TestSendAfterItsEchoTakesTheEchosTS(t *testing.T) {
	s := newWriteServer(t)
	echo := s.ev(self, "m.room.message", map[string]any{"msgtype": "m.text", "body": "hi"})
	echoTS := s.live(echo)
	s.next = []id.EventID{echo.ID}
	msg := s.ports.messages.Send(ids.ChannelID(s.alias), "hi").(ui.MessageSentMsg)
	if msg.Message.TS != echoTS {
		t.Fatalf("sent TS %q, echo's %q", msg.Message.TS, echoTS)
	}
}

func TestSendFailures(t *testing.T) {
	s := newWriteServer(t)
	s.answer = func(w http.ResponseWriter, r writeReq) bool {
		w.WriteHeader(http.StatusForbidden)
		writeJSON(t, w, map[string]any{"errcode": "M_FORBIDDEN", "error": "nope"})
		return true
	}
	if msg, ok := s.ports.messages.Send(ids.ChannelID(s.alias), "hi").(ui.MessageSendFailedMsg); !ok || msg.ChannelID != s.alias || msg.Reason == "" {
		t.Fatalf("Send = %#v", msg)
	}
	if msg, ok := s.ports.messages.Send("CNOSUCHROOM", "hi").(ui.MessageSendFailedMsg); !ok || msg.Reason == "" {
		t.Fatalf("Send to an unknown room = %#v", msg)
	}
}

func TestEditDeleteAndPermalink(t *testing.T) {
	s := newWriteServer(t)
	orig, ts := s.text(self, "helo")
	s.take()

	edited := s.ports.messages.Edit(ids.ChannelID(s.alias), ids.MessageTS(ts), "hello").(ui.MessageEditedMsg)
	if edited.Err != nil || edited.TS != ts || edited.ChannelID != s.alias {
		t.Fatalf("Edit = %+v", edited)
	}
	req := s.one("/send/m.room.message/")
	if rel := relates(req); rel["rel_type"] != "m.replace" || rel["event_id"] != string(orig.ID) {
		t.Errorf("m.relates_to = %v", rel)
	}
	if nc, _ := req.body["m.new_content"].(map[string]any); nc["body"] != "hello" || req.body["body"] != "* hello" {
		t.Errorf("body = %v", req.body)
	}

	deleted := s.ports.messages.Delete(ids.ChannelID(s.alias), ids.MessageTS(ts)).(ui.MessageDeletedMsg)
	if deleted.Err != nil || deleted.TS != ts {
		t.Fatalf("Delete = %+v", deleted)
	}
	if req := s.one("/redact/"); !strings.Contains(req.path, "/redact/"+string(orig.ID)+"/") {
		t.Errorf("redaction = %+v", req)
	}
	if got := s.ports.messages.Delete(ids.ChannelID(s.alias), "0000000001.000000").(ui.MessageDeletedMsg); got.Err == nil {
		t.Errorf("Delete of an unknown TS succeeded")
	}

	link, err := s.ports.messages.Permalink(context.Background(), ids.ChannelID(s.alias), ids.MessageTS(ts), "")
	if want := "https://matrix.to/#/" + url.PathEscape(string(general)) + "/" + url.PathEscape(string(orig.ID)) + "?via=example.org"; err != nil || link != want {
		t.Fatalf("Permalink = %q %v, want %q", link, err, want)
	}
}

func TestSendReplyThreadsItWithAFallbackToTheNewestReply(t *testing.T) {
	s := newWriteServer(t)
	root, rootTS := s.text(alice, "root")
	reply := s.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "first",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}})
	s.live(reply)
	s.take()

	msg, ok := s.ports.threads.SendReply(ids.ChannelID(s.alias), ids.ThreadTS(rootTS), "second", false).(ui.ThreadReplySentMsg)
	if !ok || msg.Message.TS == "" || msg.Message.ThreadTS != rootTS || msg.Message.Text != "second" || msg.Broadcast {
		t.Fatalf("SendReply = %#v", msg)
	}
	rel := relates(s.one("/send/m.room.message/"))
	inReply, _ := rel["m.in_reply_to"].(map[string]any)
	if rel["rel_type"] != "m.thread" || rel["event_id"] != string(root.ID) || rel["is_falling_back"] != true || inReply["event_id"] != string(reply.ID) {
		t.Fatalf("m.relates_to = %v", rel)
	}

	// Broadcast: the reply, then the same text as a plain message.
	msg = s.ports.threads.SendReply(ids.ChannelID(s.alias), ids.ThreadTS(rootTS), "all", true).(ui.ThreadReplySentMsg)
	if !msg.Broadcast || msg.Message.Subtype != "thread_broadcast" {
		t.Fatalf("broadcast reply = %#v", msg)
	}
	got := s.take()
	if len(got) != 2 || relates(got[0])["rel_type"] != "m.thread" || got[1].body["m.relates_to"] != nil || got[1].body["body"] != "all" {
		t.Fatalf("broadcast requests = %+v", got)
	}
}

func TestMarkReadAndUnread(t *testing.T) {
	s := newWriteServer(t)
	first, firstTS := s.text(alice, "one")
	second, secondTS := s.text(alice, "two")
	s.receipt(first.ID, "")
	s.take()

	// Marking at or before the receipt sends no receipt.
	if got := s.ports.channels.MarkRead(ids.ChannelID(s.alias), ids.MessageTS(firstTS)); got != (ui.ChannelMarkedReadMsg{ChannelID: s.alias}) {
		t.Fatalf("MarkRead = %#v", got)
	}
	if got := s.take(); len(got) != 0 {
		t.Fatalf("requests for an old mark: %+v", got)
	}

	if got := s.ports.channels.MarkRead(ids.ChannelID(s.alias), ids.MessageTS(secondTS)); got != (ui.ChannelMarkedReadMsg{ChannelID: s.alias}) {
		t.Fatalf("MarkRead = %#v", got)
	}
	got := s.take()
	if len(got) != 2 || got[0].path != "/_matrix/client/v3/rooms/"+string(general)+"/receipt/m.read.private/"+string(second.ID) ||
		got[0].body["thread_id"] != "main" || got[1].body["m.fully_read"] != string(second.ID) || got[1].body["m.read"] != nil {
		t.Fatalf("mark read requests = %+v", got)
	}

	// Mark unread: room account data; the next mark read clears it.
	unread := s.ports.messages.MarkUnread(ids.ChannelID(s.alias), "", ids.MessageTS(firstTS), 1).(ui.MessageMarkedUnreadMsg)
	if unread.Err != nil || unread.BoundaryTS != firstTS || unread.UnreadCount != 1 {
		t.Fatalf("MarkUnread = %+v", unread)
	}
	req := s.one("/rooms/" + string(general) + "/account_data/m.marked_unread")
	if req.method != http.MethodPut || req.body["unread"] != true {
		t.Fatalf("mark unread = %+v", req)
	}
	resp := lsync()
	ljoin(resp, general).AccountData.Events = []*event.Event{lmk(t, "m.marked_unread", "", "", 0, nil, map[string]any{"unread": true})}
	s.apply(resp)
	s.ports.channels.MarkRead(ids.ChannelID(s.alias), ids.MessageTS(secondTS))
	if req := s.one("/account_data/m.marked_unread"); req.body["unread"] != false {
		t.Fatalf("unmark = %+v", req)
	}

	// A thread is marked unread locally only.
	if got := s.ports.messages.MarkUnread(ids.ChannelID(s.alias), ids.ThreadTS(firstTS), ids.MessageTS(firstTS), 0).(ui.MessageMarkedUnreadMsg); got.Err != nil || len(s.take()) != 0 {
		t.Fatalf("thread MarkUnread = %+v", got)
	}
}

func TestThreadMarkSendsAThreadedReceipt(t *testing.T) {
	s := newWriteServer(t)
	root, rootTS := s.text(alice, "root")
	reply := s.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "r",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}})
	replyTS := s.live(reply)
	s.take()
	got := s.ports.threads.Mark(ids.ChannelID(s.alias), ids.ThreadTS(rootTS), ids.MessageTS(replyTS))()
	if got != (ui.ThreadMarkedLocalMsg{ChannelID: s.alias, ThreadTS: rootTS, TS: replyTS}) {
		t.Fatalf("Mark = %#v", got)
	}
	req := s.one("/receipt/")
	if req.path != "/_matrix/client/v3/rooms/"+string(general)+"/receipt/m.read.private/"+string(reply.ID) || req.body["thread_id"] != string(root.ID) {
		t.Fatalf("receipt = %+v", req)
	}
	s.receipt(reply.ID, string(root.ID))
	s.ports.threads.Mark(ids.ChannelID(s.alias), ids.ThreadTS(rootTS), ids.MessageTS(replyTS))()
	if got := s.take(); len(got) != 0 {
		t.Fatalf("requests for an old thread mark: %+v", got)
	}
}

func TestReactionsUseGlyphKeysAndRemoveRedactsTheOwnOne(t *testing.T) {
	s := newWriteServer(t)
	msg, ts := s.text(alice, "hi")
	s.take()
	for _, name := range []string{"thumbsup", "+1::skin-tone-3", "partyparrot"} {
		if err := s.ports.reactions.Add(ids.ChannelID(s.alias), ids.MessageTS(ts), name); err != nil {
			t.Fatalf("Add %s: %v", name, err)
		}
	}
	var keys []string
	for _, r := range s.take() {
		rel := relates(r)
		if !strings.Contains(r.path, "/send/m.reaction/") || rel["event_id"] != string(msg.ID) || rel["rel_type"] != "m.annotation" {
			t.Fatalf("reaction = %+v", r)
		}
		keys = append(keys, rel["key"].(string))
	}
	if want := []string{"👍", "👍🏼", "partyparrot"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %q, want %q", keys, want)
	}

	// Removing redacts the account's stored reaction (here the echo with a
	// variation selector), not someone else's with the same key.
	mine := s.ev(self, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": msg.ID, "key": "👍\ufe0f"}})
	theirs := s.ev(alice, "m.reaction", map[string]any{"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": msg.ID, "key": "👍"}})
	s.live(mine)
	s.live(theirs)
	if err := s.ports.reactions.Remove(ids.ChannelID(s.alias), ids.MessageTS(ts), "+1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	var redacted []string
	for _, r := range s.take() {
		redacted = append(redacted, strings.Split(strings.SplitAfter(r.path, "/redact/")[1], "/")[0])
	}
	// The stored echo and the one this process sent ($sent001, whose
	// echo is not stored).
	if want := []string{string(mine.ID), "$sent001"}; !reflect.DeepEqual(redacted, want) {
		t.Fatalf("redacted %q, want %q", redacted, want)
	}
	// Only the not-yet-echoed custom one is known: it is redacted.
	if err := s.ports.reactions.Remove(ids.ChannelID(s.alias), ids.MessageTS(ts), "partyparrot"); err != nil || len(s.take()) != 1 {
		t.Fatalf("Remove partyparrot: %v", err)
	}
	// Nothing to remove is not an error.
	if err := s.ports.reactions.Remove(ids.ChannelID(s.alias), ids.MessageTS(ts), "tada"); err != nil || len(s.take()) != 0 {
		t.Fatalf("Remove tada: %v", err)
	}
}

func TestJoiningADirectInviteAddsItToMDirect(t *testing.T) {
	s := newWriteServer(t)
	const dm id.RoomID = "!dm:example.org"
	resp := lsync()
	resp.Rooms.Invite[dm] = &mautrix.SyncInvitedRoom{State: mautrix.SyncEventsList{Events: []*event.Event{
		lstate(t, "m.room.member", string(self), alice, map[string]any{"membership": "invite", "is_direct": true}),
		lmember(t, alice, "Alice Liddell"),
	}}}
	s.apply(resp)
	alias := s.res().RoomAlias(dm)
	s.answer = func(w http.ResponseWriter, r writeReq) bool {
		if r.method == http.MethodGet && strings.HasSuffix(r.path, "/account_data/m.direct") {
			writeJSON(t, w, map[string]any{string(bob): []string{"!old:example.org"}})
			return true
		}
		if strings.HasSuffix(r.path, "/join") {
			writeJSON(t, w, map[string]any{"room_id": dm})
			return true
		}
		return false
	}
	got := s.ports.channels.Join(ids.ChannelID(alias), "")
	if got != (ui.ChannelJoinedMsg{ID: alias, Name: "Alice Liddell"}) {
		t.Fatalf("Join = %#v", got)
	}
	reqs := s.take()
	if len(reqs) != 3 || reqs[0].path != "/_matrix/client/v3/rooms/"+string(dm)+"/join" || reqs[2].method != http.MethodPut {
		t.Fatalf("requests = %+v", reqs)
	}
	want := map[string]any{string(bob): []any{"!old:example.org"}, string(alice): []any{string(dm)}}
	if !reflect.DeepEqual(reqs[2].body, want) {
		t.Fatalf("m.direct = %v, want %v", reqs[2].body, want)
	}

	// A room that is not a direct invite leaves m.direct alone.
	s.ports.channels.Join(ids.ChannelID(s.alias), "")
	if reqs := s.take(); len(reqs) != 1 || !strings.HasSuffix(reqs[0].path, "/join") {
		t.Fatalf("plain join requests = %+v", reqs)
	}
}

func TestOpenConversation(t *testing.T) {
	s := newWriteServer(t)
	const dm id.RoomID = "!new:example.org"
	aliceAlias := s.res().UserAlias(alice)
	s.answer = func(w http.ResponseWriter, r writeReq) bool {
		switch {
		case strings.HasSuffix(r.path, "/createRoom"):
			writeJSON(t, w, map[string]any{"room_id": dm})
		case r.method == http.MethodGet && strings.HasSuffix(r.path, "/account_data/m.direct"):
			w.WriteHeader(http.StatusNotFound)
			writeJSON(t, w, map[string]any{"errcode": "M_NOT_FOUND"})
		case r.method == http.MethodPut && strings.HasSuffix(r.path, "/account_data/m.direct"):
			// /sync brings the room and m.direct a little later.
			go func() {
				time.Sleep(50 * time.Millisecond)
				resp := lsync()
				ljoin(resp, dm).State.Events = []*event.Event{lmember(t, self, "Me"), lmember(t, alice, "Alice Liddell")}
				resp.AccountData.Events = []*event.Event{lmk(t, "m.direct", "", "", 0, nil, map[string]any{string(alice): []string{string(dm)}})}
				if _, err := s.m.Apply(context.Background(), session.SyncBatch{Resp: resp}); err != nil {
					t.Errorf("Apply: %v", err)
				}
			}()
			writeJSON(t, w, map[string]any{})
		default:
			return false
		}
		return true
	}
	got := s.ports.channels.OpenConversation([]string{aliceAlias}, 7)()
	alias := s.res().RoomAlias(dm)
	opened, ok := got.(ui.NewMessageOpenedMsg)
	if !ok || opened.ChannelID != alias || opened.AlreadyOpen || opened.RequestID != 7 || !reflect.DeepEqual(opened.UserIDs, []string{aliceAlias}) {
		t.Fatalf("OpenConversation = %#v", got)
	}
	if r, ok := s.m.Room(alias); !ok || r.DMUser != alice {
		t.Fatalf("room when it answered: %+v %v", r, ok)
	}
	reqs := s.take()
	create := reqs[0].body
	if create["is_direct"] != true || create["preset"] != "trusted_private_chat" || !reflect.DeepEqual(create["invite"], []any{string(alice)}) {
		t.Fatalf("createRoom = %v", create)
	}
	if st, _ := create["initial_state"].([]any); len(st) != 1 || st[0].(map[string]any)["type"] != "m.room.encryption" {
		t.Fatalf("initial_state = %v", create["initial_state"])
	}
	if put := reqs[len(reqs)-1]; !reflect.DeepEqual(put.body, map[string]any{string(alice): []any{string(dm)}}) {
		t.Fatalf("m.direct = %+v", put)
	}

	// Now there is a DM with alice: it is opened, nothing is created.
	got = s.ports.channels.OpenConversation([]string{aliceAlias}, 8)()
	if !reflect.DeepEqual(got, ui.NewMessageOpenedMsg{ChannelID: alias, AlreadyOpen: true, UserIDs: []string{aliceAlias}, RequestID: 8}) {
		t.Fatalf("second OpenConversation = %#v", got)
	}
	if reqs := s.take(); len(reqs) != 0 {
		t.Fatalf("requests = %+v", reqs)
	}

	if got, ok := s.ports.channels.OpenConversation([]string{"UNOBODY0000"}, 9)().(ui.NewMessageFailedMsg); !ok || got.RequestID != 9 {
		t.Fatalf("unknown user: %#v", got)
	}
}

// stateStore gives the backend's client mautrix's state store on the
// test database (the one the room model writes members to), with general
// encrypted, and answers /members with self, alice, bob and carol.
func (s *writeServer) stateStore() {
	s.t.Helper()
	ctx := context.Background()
	ss := sqlstatestore.NewSQLStateStore(s.st.DB, nil, false)
	if err := ss.SetEncryptionEvent(ctx, general, &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}); err != nil {
		s.t.Fatal(err)
	}
	s.b.client.StateStore = ss
	s.answer = func(w http.ResponseWriter, r writeReq) bool {
		if !strings.HasSuffix(r.path, "/rooms/"+string(general)+"/members") {
			return false
		}
		var chunk []map[string]any
		for _, u := range []id.UserID{self, alice, bob, carol} {
			e := wire(lmember(s.t, u, ""))
			e["state_key"] = u
			chunk = append(chunk, e)
		}
		writeJSON(s.t, w, map[string]any{"chunk": chunk})
		return true
	}
}

const carol id.UserID = "@carol:example.org"

func TestEncryptedSendLoadsTheMembersFirstOnce(t *testing.T) {
	s := newWriteServer(t)
	s.stateStore()
	for range 2 {
		if _, ok := s.ports.messages.Send(ids.ChannelID(s.alias), "hi").(ui.MessageSentMsg); !ok {
			t.Fatal("Send failed")
		}
	}
	var paths []string
	for _, r := range s.take() {
		paths = append(paths, r.method+" "+r.path[strings.LastIndex(r.path, "/rooms/"):])
	}
	want := []string{
		"GET /rooms/" + string(general) + "/members",
		"PUT /rooms/" + string(general) + "/send/m.room.message",
		"PUT /rooms/" + string(general) + "/send/m.room.message",
	}
	for i := range paths {
		if i < len(want) && strings.HasPrefix(paths[i], want[i]) {
			continue
		}
		t.Fatalf("requests = %q, want %q", paths, want)
	}
	if len(paths) != len(want) {
		t.Fatalf("requests = %q, want %q", paths, want)
	}
	members, err := s.b.client.StateStore.GetRoomJoinedOrInvitedMembers(context.Background(), general)
	if err != nil || !slices.Contains(members, carol) {
		t.Fatalf("members after the send = %v %v", members, err)
	}

	// An unencrypted room loads nothing.
	const plain id.RoomID = "!plain:example.org"
	resp := lsync()
	ljoin(resp, plain).State.Events = []*event.Event{lmember(t, self, "Me")}
	s.apply(resp)
	s.ports.messages.Send(ids.ChannelID(s.res().RoomAlias(plain)), "hi")
	if reqs := s.take(); len(reqs) != 1 || strings.HasSuffix(reqs[0].path, "/members") {
		t.Fatalf("requests in a plain room = %+v", reqs)
	}
}

func TestMembershipFetchTellsTheUI(t *testing.T) {
	s := newWriteServer(t)
	s.stateStore()
	// Without a sender it still loads the members.
	s.ports.channels.MembershipFetch(ids.ChannelID(s.alias))
	if reqs := s.take(); len(reqs) != 1 || !strings.HasSuffix(reqs[0].path, "/members") {
		t.Fatalf("requests = %+v", reqs)
	}
	var sent []tea.Msg
	s.b.write.setSend(func(m tea.Msg) { sent = append(sent, m) })
	s.ports.channels.MembershipFetch(ids.ChannelID(s.alias))
	if reqs := s.take(); len(reqs) != 0 {
		t.Fatalf("requests on the second fetch = %+v", reqs)
	}
	if len(sent) != 1 {
		t.Fatalf("sent = %#v", sent)
	}
	got := sent[0].(ui.ChannelMembershipMsg)
	slices.Sort(got.MemberIDs)
	var want []string
	for _, u := range []id.UserID{self, alice, bob, carol} {
		want = append(want, s.res().UserAlias(u))
	}
	slices.Sort(want)
	if got.ChannelID != s.alias || !reflect.DeepEqual(got.MemberIDs, want) {
		t.Fatalf("membership = %+v, want %q", got, want)
	}
}

func TestTypingAndTheStoreOnlyBackend(t *testing.T) {
	s := newWriteServer(t)
	s.b.sendTyping(s.alias)
	req := s.one("/typing/")
	if req.method != http.MethodPut || req.path != "/_matrix/client/v3/rooms/"+string(general)+"/typing/"+string(self) ||
		req.body["typing"] != true || req.body["timeout"] != float64(typingTimeout.Milliseconds()) {
		t.Fatalf("typing = %+v", req)
	}

	// Without a client every write fails without a request.
	h := newHarness(t, nil)
	var p ports
	h.b.fillWrite(&p)
	if _, ok := p.messages.Send(ids.ChannelID(h.alias), "hi").(ui.MessageSendFailedMsg); !ok {
		t.Fatal("Send without a client did not fail")
	}
	if err := p.reactions.Add(ids.ChannelID(h.alias), "1", "+1"); err == nil {
		t.Fatal("Add without a client did not fail")
	}
	h.b.sendTyping(h.alias)
}
