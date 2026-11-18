package svc

import (
	"net/url"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
)

// TestResolveLink: matrix.to links to a joined room, by ID, canonical
// alias or alt alias, resolve to the room,
// its message, or a thread reply with its root, from the store alone; a
// link to an unknown or invited room, or not to matrix.to, does not.
func TestResolveLink(t *testing.T) {
	h := newHarness(t, nil)
	resp := lsync()
	ljoin(resp, general).State.Events = []*event.Event{
		lstate(t, "m.room.canonical_alias", "", alice, map[string]any{"alias": "#general:example.org",
			"alt_aliases": []string{"#lobby:other.org"}}),
	}
	resp.Rooms.Invite["!invited:example.org"] = &mautrix.SyncInvitedRoom{State: mautrix.SyncEventsList{Events: []*event.Event{
		lstate(t, "m.room.member", string(self), alice, map[string]any{"membership": "invite"}),
	}}}
	h.apply(resp)

	root, rootTS := h.text(alice, "root")
	reply := h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "reply",
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}})
	replyTS := h.live(reply)
	edit := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "* root!",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "root!"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": root.ID}})
	h.live(edit)

	to := func(parts ...string) string {
		u := "https://matrix.to/#"
		for _, p := range parts {
			u += "/" + url.PathEscape(p)
		}
		return u + "?via=example.org"
	}
	ch := ids.ChannelID(h.alias)
	cases := []struct {
		name string
		url  string
		want core.LinkTarget
		ok   bool
	}{
		{"room ID", to(string(general)), core.LinkTarget{ChannelID: ch}, true},
		{"alias", "https://matrix.to/#/%23general:example.org", core.LinkTarget{ChannelID: ch}, true},
		{"message", to(string(general), string(root.ID)), core.LinkTarget{ChannelID: ch, MessageTS: ids.MessageTS(rootTS)}, true},
		{"message by alias", to("#general:example.org", string(root.ID)), core.LinkTarget{ChannelID: ch, MessageTS: ids.MessageTS(rootTS)}, true},
		{"alt alias", to("#lobby:other.org", string(root.ID)), core.LinkTarget{ChannelID: ch, MessageTS: ids.MessageTS(rootTS)}, true},
		{"thread reply", to(string(general), string(reply.ID)),
			core.LinkTarget{ChannelID: ch, MessageTS: ids.MessageTS(replyTS), ThreadTS: ids.ThreadTS(rootTS)}, true},
		{"edit opens its message", to(string(general), string(edit.ID)), core.LinkTarget{ChannelID: ch, MessageTS: ids.MessageTS(rootTS)}, true},
		{"event not stored", to(string(general), "$nope"), core.LinkTarget{ChannelID: ch}, true},
		{"matrix: URI", "matrix:roomid/" + string(general)[1:] + "/e/" + string(root.ID)[1:], core.LinkTarget{ChannelID: ch, MessageTS: ids.MessageTS(rootTS)}, true},
		{"unknown room", to("!other:example.org"), core.LinkTarget{}, false},
		{"unknown alias", to("#other:example.org"), core.LinkTarget{}, false},
		{"invited room", to("!invited:example.org"), core.LinkTarget{}, false},
		{"user", to(string(alice)), core.LinkTarget{}, false},
		{"not matrix.to", "https://example.org/#/" + string(general), core.LinkTarget{}, false},
	}
	r := linkResolver{h.b}
	for _, c := range cases {
		got, ok := r.ResolveLink(c.url)
		if ok != c.ok || got != c.want {
			t.Errorf("%s: ResolveLink(%q) = %+v %v, want %+v %v", c.name, c.url, got, ok, c.want, c.ok)
		}
	}

	// The permalink Y copies resolves back to its message.
	link := general.EventURI(root.ID, "example.org").MatrixToURL()
	if got, ok := r.ResolveLink(link); !ok || got.MessageTS != ids.MessageTS(rootTS) {
		t.Errorf("permalink %q = %+v %v", link, got, ok)
	}
}
