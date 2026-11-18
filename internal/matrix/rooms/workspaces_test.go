package rooms

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/ui/sidebar"
)

// TestWorkspaces: Home holds the DMs; rooms in no space are grouped by
// server (the account's own first, a room ID without one falling back to
// its canonical alias); each top-level space holds the rooms under it at
// any depth, a sub-space's rooms in the sub-space's section; a room in two
// top-level spaces is in both, and spaces in a cycle with no top leave
// their rooms grouped by server.
func TestWorkspaces(t *testing.T) {
	f := newFixture(t)
	resp := world(t)
	// Engineering > Platform (a sub-space) > plat; Platform also lists
	// eng, which Engineering lists directly with order "b".
	sub := join(resp, "!platform:x")
	sub.State.Events = append(sub.State.Events,
		state(t, "m.room.create", "", alice, map[string]any{"type": "m.space"}),
		state(t, "m.room.name", "", alice, map[string]any{"name": "Platform"}),
		state(t, "m.space.child", "!plat:x", alice, map[string]any{"via": []string{"x"}, "order": "a"}),
		state(t, "m.space.child", "!eng:x", alice, map[string]any{"via": []string{"x"}, "order": "a"}),
	)
	join(resp, "!space1:x").State.Events = append(join(resp, "!space1:x").State.Events,
		state(t, "m.space.child", "!platform:x", alice, map[string]any{"via": []string{"x"}}))
	join(resp, "!plat:x").State.Events = append(join(resp, "!plat:x").State.Events,
		state(t, "m.room.name", "", alice, map[string]any{"name": "plat"}))
	// Two spaces that list each other, with a room: no top.
	for _, s := range [][2]id.RoomID{{"!loopa:x", "!loopb:x"}, {"!loopb:x", "!loopa:x"}} {
		jr := join(resp, s[0])
		jr.State.Events = append(jr.State.Events,
			state(t, "m.room.create", "", alice, map[string]any{"type": "m.space"}),
			state(t, "m.room.name", "", alice, map[string]any{"name": string(s[0])}),
			state(t, "m.space.child", string(s[1]), alice, map[string]any{"via": []string{"x"}}),
		)
	}
	join(resp, "!loopa:x").State.Events = append(join(resp, "!loopa:x").State.Events,
		state(t, "m.space.child", "!looped:x", alice, map[string]any{"via": []string{"x"}}))
	join(resp, "!looped:x").State.Events = append(join(resp, "!looped:x").State.Events,
		state(t, "m.room.name", "", alice, map[string]any{"name": "looped"}))
	// Rooms in no space on other servers: one by its ID, one (an ID
	// without a server, as from room version 12) by its alias.
	join(resp, "!aaa:other.org").State.Events = append(join(resp, "!aaa:other.org").State.Events,
		state(t, "m.room.name", "", alice, map[string]any{"name": "elsewhere"}))
	join(resp, "!v12room").State.Events = append(join(resp, "!v12room").State.Events,
		state(t, "m.room.name", "", alice, map[string]any{"name": "aliased"}),
		state(t, "m.room.canonical_alias", "", alice, map[string]any{"alias": "#aliased:alias.org"}))
	f.apply(resp)

	type row struct{ name, section string }
	got := map[string][]row{}
	var order []string
	sections := map[string][]sidebar.SectionMeta{}
	for _, ws := range f.m.Workspaces() {
		order = append(order, ws.Name)
		for _, c := range ws.Channels {
			got[ws.Name] = append(got[ws.Name], row{c.Name, c.Section})
		}
		sections[ws.Name] = ws.Sections
		if len(ws.Finder) != len(ws.Channels) {
			t.Errorf("%s: %d finder items for %d channels", ws.Name, len(ws.Finder), len(ws.Channels))
		}
	}
	plat := f.roomAlias("!platform:x")
	want := map[string][]row{
		"Direct Messages": {{"Alice", "dms"}, {"Bob, Carol", "dms"}},
		"x":               {{"general", "rooms"}, {"looped", "rooms"}, {"secret", "rooms"}},
		"alias.org":       {{"aliased", "rooms"}},
		"other.org":       {{"elsewhere", "rooms"}},
		"Alpha":           {{"both", "rooms"}, {"tie", "rooms"}},
		// eng: Platform's order "a" beats Engineering's "b".
		"Engineering": {{"eng", plat}, {"plat", plat}, {"both", "rooms"}, {"tie", "rooms"}},
	}
	if !reflect.DeepEqual(order, []string{"Direct Messages", "x", "alias.org", "other.org", "Alpha", "Engineering"}) {
		t.Errorf("workspaces = %q", order)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%v\nwant\n%v", got, want)
	}
	wantEng := []sidebar.SectionMeta{{ID: plat, Name: "Platform", Type: "standard"}, {ID: "rooms", Name: "Rooms", Type: "channels"}}
	if !reflect.DeepEqual(sections["Engineering"], wantEng) {
		t.Errorf("Engineering sections = %+v", sections["Engineering"])
	}
	if ws := f.m.Workspaces(); ws[0].ID != "" || ws[1].ID != "@x" || ws[5].ID != f.roomAlias("!space1:x") {
		t.Errorf("IDs = %q, %q, %q", ws[0].ID, ws[1].ID, ws[5].ID)
	}
}

// TestWorkspacesInvites: a pending invite is a row of an "Invites"
// section, first, in the workspace the room joins: a direct invite in
// Direct Messages, a room a joined space lists in that space's, any other
// in its server's (which the invite alone adds to the rail). Rows carry
// Invite and the inviter's name, also after a restart, and have no
// finder entry; an invited space is a row marked InviteSpace. Joining
// moves the row to its section.
func TestWorkspacesInvites(t *testing.T) {
	f := newFixture(t)
	f.apply(world(t))
	invite := func(resp *mautrix.RespSync, rid id.RoomID, from id.UserID, direct bool, extra ...*event.Event) {
		evs := []*event.Event{
			state(t, "m.room.member", string(me), from, map[string]any{"membership": "invite", "is_direct": direct}),
			state(t, "m.room.member", string(from), from, map[string]any{"membership": "join", "displayname": "Carol"}),
		}
		resp.Rooms.Invite[rid] = &mautrix.SyncInvitedRoom{State: mautrix.SyncEventsList{Events: append(evs, extra...)}}
	}
	resp := newSync()
	invite(resp, "!dminv:x", carol, true)
	invite(resp, "!roominv:x", carol, false, state(t, "m.room.name", "", carol, map[string]any{"name": "party"}))
	invite(resp, "!far:far.org", carol, false, state(t, "m.room.name", "", carol, map[string]any{"name": "faraway"}))
	invite(resp, "!child:x", carol, false, state(t, "m.room.name", "", carol, map[string]any{"name": "kid"}))
	invite(resp, "!spaceinv:x", carol, false, state(t, "m.room.create", "", carol, map[string]any{"type": "m.space"}),
		state(t, "m.room.name", "", carol, map[string]any{"name": "club"}))
	join(resp, "!space2:x").State.Events = append(join(resp, "!space2:x").State.Events,
		state(t, "m.space.child", "!child:x", alice, map[string]any{"via": []string{"x"}}))
	f.apply(resp)

	type row struct {
		name, section, inviter string
		invite, space          bool
	}
	rows := func() (map[string][]row, map[string]sidebar.SectionMeta) {
		got := map[string][]row{}
		firstSection := map[string]sidebar.SectionMeta{}
		for _, ws := range f.m.Workspaces() {
			for _, c := range ws.Channels {
				if c.Invite || c.Section == "invites" {
					got[ws.Name] = append(got[ws.Name], row{c.Name, c.Section, c.Inviter, c.Invite, c.InviteSpace})
				}
			}
			for _, fi := range ws.Finder {
				if fi.Name == "Carol" || fi.Name == "party" || fi.Name == "faraway" || fi.Name == "kid" || fi.Name == "club" {
					t.Errorf("%s: invite %q in the finder", ws.Name, fi.Name)
				}
			}
			firstSection[ws.Name] = ws.Sections[0]
		}
		for _, rs := range got {
			slices.SortFunc(rs, func(a, b row) int { return strings.Compare(a.name, b.name) })
		}
		return got, firstSection
	}
	got, firstSection := rows()
	want := map[string][]row{
		"Direct Messages": {{"Carol", "invites", "Carol", true, false}},
		"x":               {{"club", "invites", "Carol", true, true}, {"party", "invites", "Carol", true, false}},
		"far.org":         {{"faraway", "invites", "Carol", true, false}},
		"Alpha":           {{"kid", "invites", "Carol", true, false}},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("invite rows =\n%v\nwant\n%v", got, want)
	}
	f.m = f.reopen()
	if after, _ := rows(); !reflect.DeepEqual(after, want) {
		t.Errorf("invite rows after a restart =\n%v\nwant\n%v", after, want)
	}
	inv := sidebar.SectionMeta{ID: "invites", Name: "Invites", Type: "invites"}
	for _, ws := range []string{"Direct Messages", "x", "far.org", "Alpha"} {
		if firstSection[ws] != inv {
			t.Errorf("%s: first section %+v, want Invites", ws, firstSection[ws])
		}
	}
	if firstSection["Engineering"] == inv {
		t.Error("Engineering has an Invites section without invites")
	}

	// Joining the room invite moves it to Rooms.
	resp = newSync()
	jr := join(resp, "!roominv:x")
	jr.State.Events = append(jr.State.Events, state(t, "m.room.name", "", carol, map[string]any{"name": "party"}))
	f.apply(resp)
	for _, ws := range f.m.Workspaces() {
		if ws.Name != "x" {
			continue
		}
		for _, c := range ws.Channels {
			if c.Name == "party" && (c.Invite || c.Section != "rooms") {
				t.Errorf("joined invite = %+v", c)
			}
		}
	}
}
