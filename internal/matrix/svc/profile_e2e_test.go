//go:build matrixe2e

package svc

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// TestProfileAndPresenceAgainstHomeserver looks up a stranger's profile
// and sets alice's presence.
func TestProfileAndPresenceAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	ctx := e2e.Ctx(t)
	if err := bob.Client.SetDisplayName(ctx, "Bob Builder"); err != nil {
		t.Fatalf("SetDisplayName: %v", err)
	}

	creds, err := session.Login(ctx, srv.URL, aliceID.String(), password, "slk (profile e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ids := ident.New(st)
	s := startSyncing(t, creds, st, ids)
	b := s.b
	sent := make(chan tea.Msg, 10)
	b.SetUISend(func(m tea.Msg) { sent <- m })
	var p ports
	b.fillProfile(&p)

	// A stranger's profile comes from /profile.
	bobAlias := s.m.Resolver().UserAlias(bob.Client.UserID)
	got, err := p.profiles.Profile(ctx, b.TeamID(), bobAlias)
	if err != nil || got.DisplayName != "Bob Builder" || got.Handle != bob.Client.UserID.String() {
		t.Fatalf("Profile of a stranger = %+v, %v", got, err)
	}

	// Away and Active set alice's presence, and Away holds while her
	// device keeps syncing: a /sync without set_presence would mark her
	// online again (Synapse). Bob sees her presence from a room they
	// share.
	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "presence", Invite: []id.UserID{aliceID}})
	if _, err := b.client.JoinRoomByID(ctx, room); err != nil {
		t.Fatalf("join: %v", err)
	}
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	presence := func() event.Presence {
		resp, err := bob.Client.GetPresence(context.Background(), aliceID)
		if err != nil {
			t.Fatalf("GetPresence: %v", err)
		}
		return resp.Presence
	}
	p.presence.SetStatus(core.PresenceSetAway, 0)
	e2e.Eventually(t, "alice unavailable", func() bool { return presence() == event.PresenceUnavailable })
	for i := range 3 {
		body := fmt.Sprintf("while away %d", i)
		bob.SendText(t, room, body)
		e2e.Eventually(t, "alice syncs "+body, func() bool {
			var n int
			err := st.DB.QueryRow(ctx, "SELECT count(*) FROM slkmx_events WHERE body = $1", body).Scan(&n)
			return err == nil && n == 1
		})
	}
	if got := presence(); got != event.PresenceUnavailable {
		t.Fatalf("presence after alice synced while away = %s, want unavailable", got)
	}
	p.presence.SetStatus(core.PresenceSetActive, 0)
	e2e.Eventually(t, "alice online", func() bool { return presence() == event.PresenceOnline })
	// Accepted presence is told to the UI for every workspace; nothing
	// else (no toast, no undo) is sent.
	for {
		select {
		case m := <-sent:
			if _, ok := m.(ui.StatusChangeMsg); !ok {
				t.Errorf("sent %#v", m)
			}
		default:
			return
		}
	}
}
