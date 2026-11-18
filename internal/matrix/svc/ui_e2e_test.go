//go:build matrixe2e

package svc

import (
	"slices"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/sidebar"
	"github.com/gammons/slk/internal/ui/workspace"
)

// sidebarRow waits for a SectionsRefreshedMsg of team in which the row
// for alias satisfies ok (nil row: alias is not in the sidebar).
func (w *writeE2E) sidebarRow(what, team, alias string, ok func(row *sidebar.ChannelItem) bool) {
	w.t.Helper()
	w.wait(what, func(msg tea.Msg) bool {
		m, isRefresh := msg.(ui.SectionsRefreshedMsg)
		if !isRefresh || m.TeamID != team {
			return false
		}
		i := slices.IndexFunc(m.Channels, func(c sidebar.ChannelItem) bool { return c.ID == alias })
		if i < 0 {
			return ok(nil)
		}
		return ok(&m.Channels[i])
	})
}

// TestInvitesAndLinksAgainstHomeserver: invites show in the Invites
// section of the workspace they belong in, joining one moves it to its
// section (a space to the rail) and declining one drops it; matrix.to links (the permalinks
// Y copies) resolve to the room, message and thread from the store, also
// for a room outside the workspace on screen.
func TestInvitesAndLinksAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	w := startWrite(t, srv, aliceID, password, nil)
	ctx := e2e.Ctx(t)
	isInvite := func(r *sidebar.ChannelItem) bool {
		return r != nil && r.Invite && r.Section == "invites" && r.Inviter != ""
	}

	// A room invite is in its server's workspace; joining moves it to
	// Rooms.
	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "party", Invite: []id.UserID{aliceID}})
	alias := w.room(room, nil).Alias
	// Its server's: the one in its ID, none from room version 12 on
	// (rooms.Workspace).
	_, srvName, _ := strings.Cut(string(room), ":")
	serverTeam := w.b.teamOf("@" + srvName)
	w.sidebarRow("the room invite", serverTeam, alias, isInvite)
	if got := w.p.channels.Join(ids.ChannelID(alias), "party"); got != (ui.ChannelJoinedMsg{ID: alias, Name: "party"}) {
		t.Fatalf("Join = %#v", got)
	}
	w.sidebarRow("the joined room in Rooms", serverTeam, alias, func(r *sidebar.ChannelItem) bool {
		return r != nil && !r.Invite && r.Section == "rooms"
	})

	// A direct invite is in Direct Messages; declining drops it.
	dm := bob.CreateDM(t, aliceID, true)
	dmAlias := w.room(dm, func(r rooms.Room) bool { return r.Kind == "dm" }).Alias
	w.sidebarRow("the direct invite", w.b.teamID, dmAlias, func(r *sidebar.ChannelItem) bool {
		return isInvite(r) && r.Type == "dm"
	})
	if got := w.p.channels.DeclineInvite(ids.ChannelID(dmAlias)); got != (ui.InviteDeclinedMsg{ChannelID: dmAlias}) {
		t.Fatalf("DeclineInvite = %#v", got)
	}
	bob.WaitMember(t, dm, aliceID, event.MembershipLeave)
	w.sidebarRow("the declined invite gone", w.b.teamID, dmAlias, func(r *sidebar.ChannelItem) bool { return r == nil })

	// A space invite is a row too, marked as a space; joining puts the
	// space on the rail.
	space, err := bob.Client.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name: "club", Invite: []id.UserID{aliceID},
		CreationContent: map[string]any{"type": "m.space"},
	})
	if err != nil {
		t.Fatalf("create space: %v", err)
	}
	spaceAlias := w.room(space.RoomID, nil).Alias
	_, spaceSrv, _ := strings.Cut(string(space.RoomID), ":")
	w.sidebarRow("the space invite", w.b.teamOf("@"+spaceSrv), spaceAlias, func(r *sidebar.ChannelItem) bool {
		return isInvite(r) && r.InviteSpace && r.Name == "club"
	})
	if got := w.p.channels.Join(ids.ChannelID(spaceAlias), "club"); got != (ui.ChannelJoinedMsg{ID: spaceAlias, Name: "club"}) {
		t.Fatalf("Join space = %#v", got)
	}
	w.wait("the space on the rail", func(msg tea.Msg) bool {
		set, ok := msg.(ui.WorkspacesSetMsg)
		return ok && slices.ContainsFunc(set.Items, func(it workspace.WorkspaceItem) bool { return it.Name == "club" })
	})

	// A room by its canonical alias, alice not invited yet: links to it
	// stay in the browser until she has joined.
	aliasName := "linked" + string(aliceID.Localpart())
	created, err := bob.Client.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Name: "linked", RoomAliasName: aliasName, Preset: "public_chat",
	})
	if err != nil {
		t.Fatalf("create linked: %v", err)
	}
	roomAlias := id.NewRoomAlias(aliasName, aliceID.Homeserver())
	links := linkResolver{w.b}
	if _, ok := links.ResolveLink(roomAlias.URI().MatrixToURL()); ok {
		t.Fatal("a room alice is not in resolved")
	}
	linked := created.RoomID
	if _, err := w.s.Client().JoinRoomByID(ctx, linked); err != nil {
		t.Fatalf("join linked: %v", err)
	}
	linkedAlias := w.room(linked, func(r rooms.Room) bool { return r.Section != "" }).Alias

	// An alternative alias added later resolves too.
	altAlias := id.NewRoomAlias("alt"+aliasName, aliceID.Homeserver())
	if _, err := bob.Client.CreateAlias(ctx, altAlias, linked); err != nil {
		t.Fatalf("create alt alias: %v", err)
	}
	if _, err := bob.Client.SendStateEvent(ctx, linked, event.StateCanonicalAlias, "", map[string]any{
		"alias": roomAlias, "alt_aliases": []id.RoomAlias{altAlias},
	}); err != nil {
		t.Fatalf("set alt_aliases: %v", err)
	}
	e2e.Eventually(t, "the alt alias resolves", func() bool {
		got, ok := links.ResolveLink(altAlias.URI().MatrixToURL())
		return ok && got.ChannelID == ids.ChannelID(linkedAlias)
	})

	t.Run("matrix.to links", func(t *testing.T) {
		w.b.fetch(alias, nil) // the UI opens it
		root := bob.SendText(t, room, "the root")
		rootTS := w.echo(alias, "the root").Message.TS
		bob.SendThreadReply(t, room, root, "a reply")
		replyTS := w.wait("the reply's echo", func(msg tea.Msg) bool {
			n, ok := msg.(ui.NewMessageMsg)
			return ok && n.Message.Text == "a reply"
		}).(ui.NewMessageMsg).Message.TS

		link := func(ts string) string {
			l, err := w.p.messages.Permalink(ctx, ids.ChannelID(alias), ids.MessageTS(ts), "")
			if err != nil {
				t.Fatalf("Permalink: %v", err)
			}
			return l
		}
		cases := []struct {
			url  string
			want core.LinkTarget
		}{
			{link(rootTS), core.LinkTarget{ChannelID: ids.ChannelID(alias), MessageTS: ids.MessageTS(rootTS)}},
			{link(replyTS), core.LinkTarget{ChannelID: ids.ChannelID(alias), MessageTS: ids.MessageTS(replyTS), ThreadTS: ids.ThreadTS(rootTS)}},
			{room.URI(aliceID.Homeserver()).MatrixToURL(), core.LinkTarget{ChannelID: ids.ChannelID(alias)}},
			{roomAlias.URI().MatrixToURL(), core.LinkTarget{ChannelID: ids.ChannelID(linkedAlias)}},
			{room.EventURI("$notstored", aliceID.Homeserver()).MatrixToURL(), core.LinkTarget{ChannelID: ids.ChannelID(alias)}},
		}
		for _, c := range cases {
			if got, ok := links.ResolveLink(c.url); !ok || got != c.want {
				t.Errorf("ResolveLink(%s) = %+v %v, want %+v", c.url, got, ok, c.want)
			}
		}
	})

	t.Run("a link into another workspace", func(t *testing.T) {
		// Direct Messages on screen (a switch forgets the room opened
		// before); the link's room is in the server's workspace. The UI
		// opens it (Fetch) where it is, and its live messages reach the
		// UI there.
		if msg := w.b.switchTo(w.b.teamID); msg == nil {
			t.Fatal("switch to Direct Messages failed")
		}
		target, ok := links.ResolveLink(room.URI(aliceID.Homeserver()).MatrixToURL())
		if !ok {
			t.Fatal("room link did not resolve")
		}
		if loaded, ok := w.b.fetch(string(target.ChannelID), nil).(ui.MessagesLoadedMsg); !ok || loaded.ChannelID != alias {
			t.Fatalf("Fetch = %#v", loaded)
		}
		bob.SendText(t, room, "after the jump")
		w.echo(alias, "after the jump")
	})
}
