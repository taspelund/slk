//go:build matrixe2e

package svc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// writeE2E is alice's backend running against the harness, with every
// message Run sends recorded.
type writeE2E struct {
	t       *testing.T
	b       *Backend
	s       *session.Session
	m       *rooms.Model
	p       ports
	startup []tea.Msg

	mu   sync.Mutex
	sent []tea.Msg
}

// startWrite logs alice in, lets prepare act with her client before her
// first sync, then runs her session and backend until the test ends.
func startWrite(t *testing.T, srv e2e.Server, aliceID id.UserID, password string, prepare func(cli *mautrix.Client)) *writeE2E {
	t.Helper()
	ctx := e2e.Ctx(t)
	creds, err := session.Login(ctx, srv.URL, aliceID.String(), password, "slk (write e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := session.New(ctx, creds, st, zerolog.Nop())
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	if prepare != nil {
		prepare(s.Client())
	}
	idm := ident.New(st)
	m, err := rooms.New(ctx, st, idm, aliceID)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	w := &writeE2E{t: t, s: s, m: m}
	w.b = New(Deps{Account: "e2e", Session: s, Store: st, IDs: idm, Rooms: m})
	w.b.fillRead(&w.p)
	w.b.fillWrite(&w.p)

	gate := make(chan struct{})
	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("session Run: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		err := w.b.Run(runCtx, s.Events(), func(msg tea.Msg) {
			<-gate
			w.mu.Lock()
			w.sent = append(w.sent, msg)
			w.mu.Unlock()
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })
	w.startup, err = w.b.StartupMsgs(ctx)
	if err != nil {
		t.Fatalf("StartupMsgs: %v", err)
	}
	close(gate)
	return w
}

// wait waits for a message Run sent that match accepts.
func (w *writeE2E) wait(what string, match func(tea.Msg) bool) tea.Msg {
	w.t.Helper()
	var got tea.Msg
	e2e.Eventually(w.t, what, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		i := slices.IndexFunc(w.sent, match)
		if i >= 0 {
			got = w.sent[i]
		}
		return i >= 0
	})
	return got
}

// echo waits for the live NewMessageMsg of a message with text in alias.
func (w *writeE2E) echo(alias, text string) ui.NewMessageMsg {
	w.t.Helper()
	return w.wait("the echo of "+text, func(msg tea.Msg) bool {
		n, ok := msg.(ui.NewMessageMsg)
		return ok && !n.Changed && n.ChannelID == alias && n.Message.Text == text
	}).(ui.NewMessageMsg)
}

// sendOnce sends text through the port and checks the send's TS is its
// echo's and its event, returning the TS.
func (w *writeE2E) sendOnce(room id.RoomID, alias, text string) (string, id.EventID) {
	w.t.Helper()
	sent, ok := w.p.messages.Send(ids.ChannelID(alias), text).(ui.MessageSentMsg)
	if !ok || sent.Message.TS == "" || sent.Message.Text != text {
		w.t.Fatalf("Send %q = %#v", text, sent)
	}
	if echo := w.echo(alias, text); echo.Message.TS != sent.Message.TS {
		w.t.Fatalf("%q: sent TS %s, echo TS %s", text, sent.Message.TS, echo.Message.TS)
	}
	ev, ok, err := w.b.d.IDs.Event(context.Background(), room, sent.Message.TS)
	if err != nil || !ok {
		w.t.Fatalf("event of %s: %v %v", sent.Message.TS, ok, err)
	}
	return sent.Message.TS, ev
}

// direct is alice's m.direct.
func (w *writeE2E) direct() event.DirectChatsEventContent {
	w.t.Helper()
	d := event.DirectChatsEventContent{}
	if err := w.s.Client().GetAccountData(e2e.Ctx(w.t), event.AccountDataDirectChats.Type, &d); err != nil && !errors.Is(err, mautrix.MNotFound) {
		w.t.Fatalf("m.direct: %v", err)
	}
	return d
}

// room waits until alice's model has room and returns it.
func (w *writeE2E) room(rid id.RoomID, ok func(rooms.Room) bool) rooms.Room {
	w.t.Helper()
	var r rooms.Room
	e2e.Eventually(w.t, "room "+string(rid), func() bool {
		var found bool
		r, found = w.m.RoomByID(rid)
		return found && (ok == nil || ok(r))
	})
	return r
}

// TestWriteAgainstHomeserver drives alice's write ports and checks what
// bob (and, in an encrypted room, carol, who never spoke) receives.
func TestWriteAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	carol := e2e.NewPeer(t, srv, "carol")

	// An encrypted room alice joins before her first sync, with more
	// messages since carol joined than the sync's timeline holds, so the
	// lazy-loaded first sync does not tell alice about carol.
	secret := bob.CreateRoom(t, e2e.RoomOptions{Name: "secret", Encrypted: true, Invite: []id.UserID{aliceID, carol.UserID}})
	carol.Join(t, secret)
	w := startWrite(t, srv, aliceID, password, func(cli *mautrix.Client) {
		if _, err := cli.JoinRoomByID(e2e.Ctx(t), secret); err != nil {
			t.Fatalf("join secret: %v", err)
		}
		bob.WaitMember(t, secret, aliceID, event.MembershipJoin)
		bob.WaitMember(t, secret, carol.UserID, event.MembershipJoin)
		for i := range 55 {
			bob.SendText(t, secret, fmt.Sprintf("filler %d", i))
		}
	})
	ctx := e2e.Ctx(t)
	b, cli, res := w.b, w.s.Client(), w.m.Resolver()
	bobAlias := res.UserAlias(bob.UserID)
	secretAlias := w.room(secret, nil).Alias

	t.Run("encrypted send reaches a member the sync did not name", func(t *testing.T) {
		if cli.StateStore.IsInRoom(ctx, secret, carol.UserID) {
			t.Fatal("precondition: alice's state store already knows carol; the test proves nothing")
		}
		w.sendOnce(secret, secretAlias, "for everyone")
		bob.WaitText(t, secret, "for everyone")
		carol.WaitText(t, secret, "for everyone")
	})

	// A plain room alice is invited to and joins through the port.
	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "write", Invite: []id.UserID{aliceID}})
	alias := w.room(room, nil).Alias
	if got := w.p.channels.Join(ids.ChannelID(alias), ""); got != (ui.ChannelJoinedMsg{ID: alias, Name: "write"}) {
		t.Fatalf("Join = %#v", got)
	}
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	if d := w.direct(); len(d) != 0 {
		t.Fatalf("joining a room that is not a DM changed m.direct: %v", d)
	}

	ts, ev := w.sendOnce(room, alias, "hello bob")
	if got := bob.WaitText(t, room, "hello bob"); got.ID != ev {
		t.Fatalf("bob got %s, alice sent %s", got.ID, ev)
	}

	t.Run("edit", func(t *testing.T) {
		if got := w.p.messages.Edit(ids.ChannelID(alias), ids.MessageTS(ts), "hello, bob").(ui.MessageEditedMsg); got.Err != nil {
			t.Fatalf("Edit: %v", got.Err)
		}
		bob.WaitEvent(t, room, func(e *event.Event) bool {
			c := e.Content.AsMessage()
			return e.Type == event.EventMessage && c.RelatesTo.GetReplaceID() == ev && c.NewContent != nil && c.NewContent.Body == "hello, bob"
		})
	})

	t.Run("react and unreact", func(t *testing.T) {
		if err := w.p.reactions.Add(ids.ChannelID(alias), ids.MessageTS(ts), "thumbsup"); err != nil {
			t.Fatalf("Add: %v", err)
		}
		reaction := bob.WaitEvent(t, room, func(e *event.Event) bool {
			c := e.Content.AsReaction()
			return e.Type == event.EventReaction && c.RelatesTo.EventID == ev && c.RelatesTo.Key == "👍"
		})
		w.wait("the reaction's echo", func(msg tea.Msg) bool {
			return msg == ui.ReactionAddedMsg{ChannelID: alias, MessageTS: ts, UserID: res.UserAlias(aliceID), Emoji: "+1"}
		})
		if err := w.p.reactions.Remove(ids.ChannelID(alias), ids.MessageTS(ts), "+1"); err != nil {
			t.Fatalf("Remove: %v", err)
		}
		bob.WaitEvent(t, room, func(e *event.Event) bool { return e.Type == event.EventRedaction && e.Redacts == reaction.ID })
	})

	var replyTS string
	t.Run("thread reply, broadcast and thread mark", func(t *testing.T) {
		got, ok := w.p.threads.SendReply(ids.ChannelID(alias), ids.ThreadTS(ts), "in the thread", false).(ui.ThreadReplySentMsg)
		if !ok || got.Message.TS == "" || got.Message.ThreadTS != ts {
			t.Fatalf("SendReply = %#v", got)
		}
		replyTS = got.Message.TS
		reply := bob.WaitText(t, room, "in the thread")
		if rel := reply.Content.AsMessage().RelatesTo; rel.GetThreadParent() != ev || rel.GetReplyTo() != ev || !rel.IsFallingBack {
			t.Fatalf("reply relation = %+v", rel)
		}
		if echo := w.wait("the reply's echo", func(msg tea.Msg) bool {
			n, ok := msg.(ui.NewMessageMsg)
			return ok && n.Message.Text == "in the thread"
		}).(ui.NewMessageMsg); echo.Message.TS != replyTS || echo.Message.ThreadTS != ts {
			t.Fatalf("reply echo %+v, sent TS %s", echo.Message, replyTS)
		}

		if got := w.p.threads.SendReply(ids.ChannelID(alias), ids.ThreadTS(ts), "to all", true).(ui.ThreadReplySentMsg); !got.Broadcast || got.Message.TS == "" {
			t.Fatalf("broadcast SendReply = %#v", got)
		}
		threaded := bob.WaitEvent(t, room, func(e *event.Event) bool {
			c := e.Content.AsMessage()
			return e.Type == event.EventMessage && c.Body == "to all" && c.RelatesTo.GetThreadParent() == ev
		})
		if threaded.Content.Raw[broadcastKey] != true {
			t.Errorf("broadcast reply content = %v", threaded.Content.Raw)
		}
		bob.WaitEvent(t, room, func(e *event.Event) bool {
			c := e.Content.AsMessage()
			return e.Type == event.EventMessage && c.Body == "to all" && e.ID != threaded.ID && c.RelatesTo == nil &&
				e.Content.Raw[broadcastCopyKey] == string(threaded.ID)
		})
		// Alice's channel shows it once, as the thread_broadcast reply,
		// once both echoes are stored.
		e2e.Eventually(t, "both broadcast events stored", func() bool {
			var n int
			err := w.b.d.Store.DB.QueryRow(e2e.Ctx(t), "SELECT count(*) FROM slkmx_events WHERE room_id = $1 AND body = 'to all'", room).Scan(&n)
			return err == nil && n == 2
		})
		var rows []core.MessageItem
		for _, m := range w.b.fetch(alias, nil).(ui.MessagesLoadedMsg).Messages {
			if m.Text == "to all" {
				rows = append(rows, m)
			}
		}
		if len(rows) != 1 || rows[0].Subtype != "thread_broadcast" || rows[0].ThreadTS != ts {
			t.Errorf("channel rows of the broadcast = %+v", rows)
		}

		marked := w.p.threads.Mark(ids.ChannelID(alias), ids.ThreadTS(ts), ids.MessageTS(replyTS))()
		if marked != (ui.ThreadMarkedLocalMsg{ChannelID: alias, ThreadTS: ts, TS: replyTS}) {
			t.Fatalf("Mark = %#v", marked)
		}
		w.wait("the thread receipt's echo", func(msg tea.Msg) bool {
			return msg == ui.ThreadMarkedRemoteMsg{ChannelID: alias, ThreadTS: ts, LastRead: replyTS}
		})
	})

	t.Run("mark read and unread", func(t *testing.T) {
		bob.SendText(t, room, "unread one")
		unreadTS := w.echo(alias, "unread one").Message.TS
		if got := w.p.channels.MarkRead(ids.ChannelID(alias), ids.MessageTS(unreadTS)); got != (ui.ChannelMarkedReadMsg{ChannelID: alias}) {
			t.Fatalf("MarkRead = %#v", got)
		}
		w.wait("the receipt's echo", func(msg tea.Msg) bool {
			m, ok := msg.(ui.ChannelMarkedRemoteMsg)
			return ok && m.ChannelID == alias && m.TS == unreadTS
		})
		markedUnread := func() bool {
			row, _, err := b.d.Store.GetRoom(ctx, room)
			return err == nil && row.MarkedUnread
		}
		if got := w.p.messages.MarkUnread(ids.ChannelID(alias), "", ids.MessageTS(ts), 1).(ui.MessageMarkedUnreadMsg); got.Err != nil {
			t.Fatalf("MarkUnread: %v", got.Err)
		}
		e2e.Eventually(t, "the room marked unread", markedUnread)
		if !w.m.Snapshot().ReadStates[alias].HasUnread {
			t.Fatal("a room marked unread does not show unread")
		}
		w.p.channels.MarkRead(ids.ChannelID(alias), ids.MessageTS(unreadTS))
		e2e.Eventually(t, "the room unmarked", func() bool { return !markedUnread() })
	})

	t.Run("permalink", func(t *testing.T) {
		link, err := w.p.messages.Permalink(ctx, ids.ChannelID(alias), ids.MessageTS(ts), "")
		if err != nil || !strings.HasPrefix(link, "https://matrix.to/#/") || !strings.Contains(link, string(ev)[1:]) {
			t.Fatalf("Permalink = %q %v", link, err)
		}
	})

	t.Run("typing", func(t *testing.T) {
		b.sendTyping(alias)
		e2e.Eventually(t, "bob sees alice typing", func() bool { return slices.Contains(bob.Typing(t, room), aliceID) })
	})

	t.Run("membership", func(t *testing.T) {
		got := make(chan tea.Msg, 1)
		b.write.setSend(func(m tea.Msg) { got <- m })
		w.p.channels.MembershipFetch(ids.ChannelID(alias))
		m := (<-got).(ui.ChannelMembershipMsg)
		slices.Sort(m.MemberIDs)
		want := []string{res.UserAlias(aliceID), bobAlias}
		slices.Sort(want)
		if m.ChannelID != alias || !slices.Equal(m.MemberIDs, want) {
			t.Fatalf("membership = %+v, want %q", m, want)
		}
	})

	t.Run("delete", func(t *testing.T) {
		if got := w.p.messages.Delete(ids.ChannelID(alias), ids.MessageTS(ts)).(ui.MessageDeletedMsg); got.Err != nil {
			t.Fatalf("Delete: %v", got.Err)
		}
		bob.WaitEvent(t, room, func(e *event.Event) bool { return e.Type == event.EventRedaction && e.Redacts == ev })
	})

	t.Run("a message sent from the UI shows once", func(t *testing.T) {
		app := ui.NewApp()
		b.Install(app)
		drive(t, app, tea.WindowSizeMsg{Width: 140, Height: 40})
		for _, m := range w.startup {
			drive(t, app, m)
		}
		w.mu.Lock()
		before := slices.Clone(w.sent)
		w.mu.Unlock()
		for _, m := range before {
			drive(t, app, m)
		}
		drive(t, app, ui.ChannelSelectedMsg{ID: alias, Name: "write", Type: "private"})
		drive(t, app, ui.SendMessageMsg{ChannelID: alias, Text: "once only"})
		w.echo(alias, "once only")
		w.mu.Lock()
		after := slices.Clone(w.sent[len(before):])
		w.mu.Unlock()
		for _, m := range after {
			drive(t, app, m)
		}
		if n := strings.Count(ansi.Strip(app.View().Content), "once only"); n != 1 {
			t.Fatalf("own message shown %d times:\n%s", n, ansi.Strip(app.View().Content))
		}
		bob.WaitText(t, room, "once only")
	})

	t.Run("open a new encrypted DM", func(t *testing.T) {
		got, ok := w.p.channels.OpenConversation([]string{bobAlias}, 1)().(ui.NewMessageOpenedMsg)
		if !ok || got.AlreadyOpen || got.ChannelID == "" {
			t.Fatalf("OpenConversation = %#v", got)
		}
		dm, ok := res.RoomID(got.ChannelID)
		if !ok {
			t.Fatalf("no room for %s", got.ChannelID)
		}
		if r, _ := w.m.RoomByID(dm); r.Kind != store.RoomDM || r.DMUser != bob.UserID {
			t.Fatalf("new DM = %+v", r)
		}
		if enc, err := cli.StateStore.IsEncrypted(ctx, dm); err != nil || !enc {
			t.Fatalf("new DM encrypted = %v %v", enc, err)
		}
		if d := w.direct(); !slices.Contains(d[bob.UserID], dm) {
			t.Fatalf("m.direct = %v", d)
		}
		again, _ := w.p.channels.OpenConversation([]string{bobAlias}, 2)().(ui.NewMessageOpenedMsg)
		if !again.AlreadyOpen || again.ChannelID != got.ChannelID {
			t.Fatalf("second OpenConversation = %#v", again)
		}
		bob.Join(t, dm)
		e2e.Eventually(t, "alice sees bob join", func() bool { return cli.StateStore.IsInRoom(ctx, dm, bob.UserID) })
		w.sendOnce(dm, got.ChannelID, "secret dm")
		if e := bob.WaitText(t, dm, "secret dm"); !e.Mautrix.WasEncrypted {
			t.Fatal("the DM message was not encrypted")
		}
	})

	t.Run("accept a direct invite", func(t *testing.T) {
		dm := bob.CreateDM(t, aliceID, true)
		r := w.room(dm, func(r rooms.Room) bool { return r.Kind == store.RoomDM })
		if got := w.p.channels.Join(ids.ChannelID(r.Alias), ""); got != (ui.ChannelJoinedMsg{ID: r.Alias, Name: r.Name}) {
			t.Fatalf("Join = %#v", got)
		}
		if d := w.direct(); !slices.Contains(d[bob.UserID], dm) {
			t.Fatalf("m.direct = %v", d)
		}
		w.room(dm, func(r rooms.Room) bool { return r.Kind == store.RoomDM && r.Section != "" && r.DMUser == bob.UserID })
		bob.WaitMember(t, dm, aliceID, event.MembershipJoin)
	})
}
