//go:build matrixe2e

package svc

import (
	"context"
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

	// Away and Active set alice's presence. Her device stops syncing
	// first: the session's /sync sends no set_presence, which marks her
	// online again on every sync (Synapse), so Away does not hold while
	// it syncs until the session syncs with the presence chosen. Bob sees
	// her presence from a room they share.
	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "presence", Invite: []id.UserID{aliceID}})
	if _, err := b.client.JoinRoomByID(ctx, room); err != nil {
		t.Fatalf("join: %v", err)
	}
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	s.stop()
	presence := func() event.Presence {
		resp, err := bob.Client.GetPresence(context.Background(), aliceID)
		if err != nil {
			t.Fatalf("GetPresence: %v", err)
		}
		return resp.Presence
	}
	p.presence.SetStatus(core.PresenceSetAway, 0)
	e2e.Eventually(t, "alice unavailable", func() bool { return presence() == event.PresenceUnavailable })
	p.presence.SetStatus(core.PresenceSetActive, 0)
	e2e.Eventually(t, "alice online", func() bool { return presence() == event.PresenceOnline })
	select {
	case m := <-sent:
		t.Errorf("sent %#v", m)
	default:
	}
}
