//go:build matrixe2e

package svc

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/ui"
)

// TestReadReceiptsAgainstHomeserver: bob's public receipt on alice's
// message reaches alice's UI as a read marker, live and in a fresh
// Fetch; his threaded receipt marks the thread only; :seen's port lists
// him.
func TestReadReceiptsAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "receipts", Invite: []id.UserID{aliceID}})
	w := startWrite(t, srv, aliceID, password, func(cli *mautrix.Client) {
		if _, err := cli.JoinRoomByID(e2e.Ctx(t), room); err != nil {
			t.Fatalf("join: %v", err)
		}
		bob.WaitMember(t, room, aliceID, event.MembershipJoin)
		// A message, so the room is the newest and its workspace the
		// one on screen (live messages are sent for that one).
		bob.SendText(t, room, "welcome")
	})
	ctx := e2e.Ctx(t)
	alias := w.room(room, nil).Alias
	bobAlias := w.m.Resolver().UserAlias(bob.UserID)
	bobName := w.m.DisplayName(room, bob.UserID)
	helloTS, hello := w.sendOnce(room, alias, "hello bob")
	bob.WaitText(t, room, "hello bob")

	// Alice's UI, on the room (the only one, so the one it opens on).
	newApp := func() *ui.App {
		app := ui.NewApp()
		w.b.Install(app)
		drive(t, app, tea.WindowSizeMsg{Width: 140, Height: 40})
		for _, m := range w.startup {
			drive(t, app, m)
		}
		if app.ActiveChannelID() != alias {
			t.Fatalf("the UI opened %q, not the room %q", app.ActiveChannelID(), alias)
		}
		drive(t, app, w.p.channels.Fetch(ids.ChannelID(alias), ""))
		return app
	}
	marker := "Seen by " + bobName
	screen := func(app *ui.App) string { return ansi.Strip(app.View().Content) }
	app := newApp()
	if s := screen(app); !strings.Contains(s, "hello bob") || strings.Contains(s, "Seen by") {
		t.Fatalf("before bob reads:\n%s", s)
	}

	t.Run("live", func(t *testing.T) {
		bob.ReadReceipt(t, room, hello)
		moved := w.wait("bob's marker", func(msg tea.Msg) bool {
			m, ok := msg.(ui.ReadMarkersMovedMsg)
			return ok && m.ChannelID == alias && m.ThreadTS == "" &&
				slices.Contains(m.Moves, ui.SeenMove{UserID: bobAlias, TS: helloTS})
		})
		drive(t, app, moved)
		s := screen(app)
		if i, j := strings.Index(s, "hello bob"), strings.Index(s, marker); i < 0 || j < i {
			t.Fatalf("no %q under the message:\n%s", marker, s)
		}
	})

	t.Run("fresh fetch", func(t *testing.T) {
		got := w.p.channels.Fetch(ids.ChannelID(alias), "").(ui.MessagesLoadedMsg)
		i := slices.IndexFunc(got.Messages, func(m core.MessageItem) bool { return m.TS == helloTS })
		if i < 0 || !slices.Equal(got.Messages[i].SeenBy, []string{bobAlias}) {
			t.Fatalf("Fetch: %#v", got.Messages)
		}
		if s := screen(newApp()); !strings.Contains(s, marker) {
			t.Fatalf("a fresh UI has no marker:\n%s", s)
		}
	})

	rootTS, root := w.sendOnce(room, alias, "the root")
	resp, err := w.s.Client().SendMessageEvent(ctx, room, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "in the thread", RelatesTo: (&event.RelatesTo{}).SetThread(root, root),
	})
	if err != nil {
		t.Fatalf("thread reply: %v", err)
	}
	replyTS := w.echo(alias, "in the thread").Message.TS
	bob.WaitText(t, room, "in the thread")

	t.Run("threaded receipt marks the thread only", func(t *testing.T) {
		if err := bob.Client.SendReceipt(ctx, room, resp.EventID, event.ReceiptTypeRead, &mautrix.ReqSendReceipt{ThreadID: string(root)}); err != nil {
			t.Fatalf("threaded receipt: %v", err)
		}
		w.wait("bob's thread marker", func(msg tea.Msg) bool {
			m, ok := msg.(ui.ReadMarkersMovedMsg)
			return ok && m.ChannelID == alias && m.ThreadTS == rootTS &&
				slices.Contains(m.Moves, ui.SeenMove{UserID: bobAlias, TS: replyTS})
		})
		thread := w.p.threads.Fetch(ids.ChannelID(alias), ids.ThreadTS(rootTS)).(ui.ThreadRepliesLoadedMsg)
		if len(thread.Replies) != 1 || !slices.Equal(thread.Replies[0].SeenBy, []string{bobAlias}) {
			t.Fatalf("thread Fetch: %#v", thread.Replies)
		}
		channel := w.p.channels.Fetch(ids.ChannelID(alias), "").(ui.MessagesLoadedMsg)
		for _, m := range channel.Messages {
			want := []string(nil)
			if m.TS == helloTS {
				want = []string{bobAlias}
			}
			if !slices.Equal(m.SeenBy, want) {
				t.Errorf("channel message %q: markers %v, want %v", m.Text, m.SeenBy, want)
			}
		}
	})

	t.Run(":seen lists bob", func(t *testing.T) {
		svc := receiptService{w.b}
		readers, err := svc.SeenBy(alias, "", helloTS)
		if err != nil || len(readers) != 1 || readers[0].UserID != bobAlias || readers[0].Name != bobName || readers[0].ReadTime == "" {
			t.Fatalf("SeenBy(hello) = %+v, %v", readers, err)
		}
		if readers, err := svc.SeenBy(alias, rootTS, replyTS); err != nil || len(readers) != 1 || readers[0].UserID != bobAlias {
			t.Fatalf("SeenBy(reply) = %+v, %v", readers, err)
		}
		if readers, err := svc.SeenBy(alias, "", rootTS); err != nil || len(readers) != 0 {
			t.Fatalf("SeenBy(root) = %+v, %v (bob read the thread, not the channel)", readers, err)
		}
	})
}
