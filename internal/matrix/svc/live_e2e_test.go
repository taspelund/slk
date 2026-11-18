//go:build matrixe2e

package svc

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// TestLiveAgainstHomeserver runs alice's session through Run while bob
// posts, edits, reacts, redacts and types, and checks the ui messages.
func TestLiveAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	ctx := e2e.Ctx(t)

	creds, err := session.Login(ctx, srv.URL, aliceID.String(), password, "slk (live e2e)")
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
	ids := ident.New(st)
	m, err := rooms.New(ctx, st, ids, aliceID)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	b := New(Deps{Account: "e2e", Session: s, Store: st, IDs: ids, Rooms: m})

	var mu sync.Mutex
	var sent []tea.Msg
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
		err := b.Run(runCtx, s.Events(), func(msg tea.Msg) {
			<-gate
			mu.Lock()
			sent = append(sent, msg)
			mu.Unlock()
		})
		if err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("Run: %v", err)
		}
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })

	startup, err := b.StartupMsgs(ctx)
	if err != nil {
		t.Fatalf("StartupMsgs: %v", err)
	}
	close(gate)
	if len(startup) != 2 || startup[1] != (ui.ConnectionStateMsg{State: 1}) {
		t.Fatalf("startup = %#v", startup)
	}
	if ready := startup[0].(ui.WorkspaceReadyMsg); ready.TeamID != b.TeamID() || ready.UserID != b.selfAlias() || !ready.InitialActive {
		t.Fatalf("ready = %+v", ready)
	}

	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "live", Invite: []id.UserID{aliceID}})
	if _, err := s.Client().JoinRoomByID(ctx, room); err != nil {
		t.Fatalf("join: %v", err)
	}
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	res := m.Resolver()
	alias, bobAlias := res.RoomAlias(room), res.UserAlias(bob.UserID)

	// find returns the first message sent that matches.
	find := func(match func(tea.Msg) bool) (tea.Msg, bool) {
		mu.Lock()
		defer mu.Unlock()
		i := slices.IndexFunc(sent, match)
		if i < 0 {
			return nil, false
		}
		return sent[i], true
	}
	wait := func(what string, match func(tea.Msg) bool) tea.Msg {
		t.Helper()
		var got tea.Msg
		e2e.Eventually(t, what, func() bool {
			var ok bool
			got, ok = find(match)
			return ok
		})
		return got
	}

	wait("the room's sidebar row", func(msg tea.Msg) bool {
		c, ok := msg.(ui.ConversationOpenedMsg)
		return ok && c.Item.ID == alias && c.Item.Name == "live"
	})

	m1 := bob.SendText(t, room, "hello alice")
	added := wait("the message", func(msg tea.Msg) bool {
		n, ok := msg.(ui.NewMessageMsg)
		return ok && !n.Changed && n.ChannelID == alias && n.Message.Text == "hello alice"
	}).(ui.NewMessageMsg)
	if added.Message.UserID != bobAlias || added.Message.TS == "" {
		t.Fatalf("message = %+v", added.Message)
	}
	ts := added.Message.TS

	bob.SendEdit(t, room, m1, "hello, alice")
	wait("the edit", func(msg tea.Msg) bool {
		n, ok := msg.(ui.NewMessageMsg)
		return ok && n.Changed && n.Message.TS == ts && n.Message.Text == "hello, alice" && n.Message.IsEdited
	})

	reaction := bob.React(t, room, m1, "👍")
	wait("the reaction", func(msg tea.Msg) bool {
		return msg == ui.ReactionAddedMsg{ChannelID: alias, MessageTS: ts, UserID: bobAlias, Emoji: "+1"}
	})
	bob.Redact(t, room, reaction)
	wait("the reaction removed", func(msg tea.Msg) bool {
		return msg == ui.ReactionRemovedMsg{ChannelID: alias, MessageTS: ts, UserID: bobAlias, Emoji: "+1"}
	})

	bob.SendThreadReply(t, room, m1, "in a thread")
	wait("the thread reply", func(msg tea.Msg) bool {
		n, ok := msg.(ui.NewMessageMsg)
		return ok && n.Message.Text == "in a thread" && n.Message.ThreadTS == ts
	})

	m2 := bob.SendText(t, room, "delete me")
	m2ts := wait("the second message", func(msg tea.Msg) bool {
		n, ok := msg.(ui.NewMessageMsg)
		return ok && n.Message.Text == "delete me"
	}).(ui.NewMessageMsg).Message.TS
	bob.Redact(t, room, m2)
	wait("the redaction", func(msg tea.Msg) bool {
		return msg == ui.WSMessageDeletedMsg{ChannelID: alias, TS: m2ts}
	})

	if _, err := bob.Client.UserTyping(ctx, room, true, 30*time.Second); err != nil {
		t.Fatalf("typing: %v", err)
	}
	wait("typing", func(msg tea.Msg) bool {
		return msg == ui.UserTypingMsg{ChannelID: alias, UserID: bobAlias, WorkspaceID: b.TeamID()}
	})
}
