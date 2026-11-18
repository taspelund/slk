package rooms

import (
	"reflect"
	"testing"

	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/ui/sidebar"
)

// TestWorkspaces: Home holds the DMs and the rooms in no space; each
// top-level space holds the rooms under it at any depth, a sub-space's
// rooms in the sub-space's section; a room in two top-level spaces is in
// both, and spaces in a cycle with no top leave their rooms in Home.
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
		"Direct Messages": {{"Alice", "dms"}, {"Bob, Carol", "dms"}, {"general", "rooms"}, {"looped", "rooms"}, {"secret", "rooms"}},
		"Alpha":           {{"both", "rooms"}, {"tie", "rooms"}},
		// eng: Platform's order "a" beats Engineering's "b".
		"Engineering": {{"eng", plat}, {"plat", plat}, {"both", "rooms"}, {"tie", "rooms"}},
	}
	if !reflect.DeepEqual(order, []string{"Direct Messages", "Alpha", "Engineering"}) {
		t.Errorf("workspaces = %q", order)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("rows =\n%v\nwant\n%v", got, want)
	}
	wantEng := []sidebar.SectionMeta{{ID: plat, Name: "Platform", Type: "standard"}, {ID: "rooms", Name: "Rooms", Type: "channels"}}
	if !reflect.DeepEqual(sections["Engineering"], wantEng) {
		t.Errorf("Engineering sections = %+v", sections["Engineering"])
	}
	if ws := f.m.Workspaces(); ws[0].ID != "" || ws[2].ID != f.roomAlias("!space1:x") {
		t.Errorf("IDs = %q, %q", ws[0].ID, ws[2].ID)
	}
}
