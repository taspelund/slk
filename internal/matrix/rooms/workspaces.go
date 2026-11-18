package rooms

import (
	"maps"
	"slices"
	"strings"

	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/text"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// HomeName names the first workspace, which holds the direct chats.
const HomeName = "Direct Messages"

// Workspace is one entry of the workspace rail and the sidebar it shows.
//
// The first is Home (ID ""): the direct chats, in section "Direct
// Messages". Then the joined rooms in no space, one workspace per server
// (ID "@" plus the server name, named by it; the account's own server
// first, then by name), in section "Rooms": a room's server is the one in
// its canonical alias, else (a chat named after its members) the
// account's own when a member is on it, else the one in its ID, else none
// ("Other rooms"); see serverOf. Then
// each top-level space (a joined space with no joined parent space), by
// name, then alias, with ID its room alias: the rooms under it, at any
// depth, each in the section of its nearest parent space below the top
// (a sub-space's name, sorted by name) or, when that parent is the top
// itself, in "Rooms" after them. A room under two top-level spaces is in
// both. Rooms whose only spaces form a cycle with no top count as in no
// space.
//
// Pending invites, to rooms and to spaces, are rows of an "Invites"
// section (ID "invites", type "invites"), first in the workspace the
// room would be in once joined: a direct invite in Direct Messages, a
// room or space under a joined space in that space's top-level
// workspace, any other in its server's (which an invite alone can add to
// the rail). Their ChannelItem has Invite set, InviteSpace for a space,
// and Inviter the inviter's display name when known (the invite's
// sender is stored with the room); they have no Finder entry. A joined
// space is a rail workspace, not a row.
type Workspace struct {
	ID       string
	Name     string
	Channels []sidebar.ChannelItem
	Finder   []core.ChannelFinderItem
	Sections []sidebar.SectionMeta
}

// Workspaces returns the rail's workspaces, Home first. Each call builds
// new slices.
func (m *Model) Workspaces() []Workspace {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.workspacesLocked()
}

// workspacesLocked is Workspaces; m.mu is held.
func (m *Model) workspacesLocked() []Workspace {
	// parents are a room's joined parent spaces, with their orders.
	parents := func(rid id.RoomID) map[*room]string {
		out := map[*room]string{}
		for sid, order := range m.edges[rid] {
			if s := m.rooms[sid]; s != nil && s.row.IsSpace && s.row.JoinState == store.Joined && sid != rid {
				out[s] = order
			}
		}
		return out
	}
	// tops are the top-level spaces above a room or space.
	topsOf := map[id.RoomID]map[*room]bool{}
	var tops func(rid id.RoomID, seen map[id.RoomID]bool) map[*room]bool
	tops = func(rid id.RoomID, seen map[id.RoomID]bool) map[*room]bool {
		if t, ok := topsOf[rid]; ok {
			return t
		}
		out := map[*room]bool{}
		seen[rid] = true
		for p := range parents(rid) {
			if seen[p.row.ID] {
				continue
			}
			if len(parents(p.row.ID)) == 0 {
				out[p] = true
				continue
			}
			for t := range tops(p.row.ID, seen) {
				out[t] = true
			}
		}
		delete(seen, rid)
		topsOf[rid] = out
		return out
	}
	// under says whether space s is top or below it.
	under := func(s, top *room) bool { return s == top || tops(s.row.ID, map[id.RoomID]bool{})[top] }

	type entry struct {
		r       *room
		section string
	}
	home := []entry{}
	byServer := map[string][]entry{}
	byTop := map[*room][]entry{}
	var topList []*room
	for _, r := range m.rooms {
		if r.row.IsSpace && r.row.JoinState == store.Joined && len(parents(r.row.ID)) == 0 {
			if _, ok := byTop[r]; !ok {
				byTop[r] = nil
				topList = append(topList, r)
			}
		}
	}
	for _, r := range m.rooms {
		invite := r.invite()
		if !r.sidebar() && !invite {
			continue
		}
		// section is the section of a joined room, or of an invite.
		section := func(joined string) string {
			if invite {
				return sectionInvites
			}
			return joined
		}
		if r.row.Kind == store.RoomDM || r.row.Kind == store.RoomGroupDM {
			home = append(home, entry{r, section(sectionDMs)})
			continue
		}
		ts := tops(r.row.ID, map[id.RoomID]bool{})
		if len(ts) == 0 {
			srv := serverOf(r, m.self.Homeserver())
			byServer[srv] = append(byServer[srv], entry{r, section(sectionRooms)})
			continue
		}
		ps := parents(r.row.ID)
		for top := range ts {
			if invite {
				byTop[top] = append(byTop[top], entry{r, sectionInvites})
				continue
			}
			var best *room
			var bestOrder string
			for p, order := range ps {
				if under(p, top) && (best == nil || spaceBefore(order, p, bestOrder, best)) {
					best, bestOrder = p, order
				}
			}
			sec := sectionRooms
			if best != nil && best != top {
				sec = best.alias
			}
			byTop[top] = append(byTop[top], entry{r, sec})
		}
	}
	slices.SortFunc(topList, func(a, b *room) int {
		if na, nb := text.Fold(a.row.Name), text.Fold(b.row.Name); na != nb {
			return cmpString(na, nb)
		}
		return cmpString(a.alias, b.alias)
	})

	build := func(ws *Workspace, entries []entry, sections []sidebar.SectionMeta) {
		if slices.ContainsFunc(entries, func(e entry) bool { return e.section == sectionInvites }) {
			sections = append([]sidebar.SectionMeta{{ID: sectionInvites, Name: "Invites", Type: "invites"}}, sections...)
		}
		rank := map[string]int{}
		for i, s := range sections {
			rank[s.ID] = i
		}
		slices.SortFunc(entries, func(a, b entry) int {
			if rank[a.section] != rank[b.section] {
				return rank[a.section] - rank[b.section]
			}
			if na, nb := text.Fold(a.r.row.Name), text.Fold(b.r.row.Name); na != nb {
				return cmpString(na, nb)
			}
			return cmpString(a.r.alias, b.r.alias)
		})
		ws.Sections = sections
		for _, e := range entries {
			r := e.r
			dm := ""
			if r.row.DMUserID != "" {
				dm, _ = m.cachedUserAlias(r.row.DMUserID)
			}
			item := sidebar.ChannelItem{
				ID: r.alias, Name: r.row.Name, Type: string(r.row.Kind), Section: e.section,
				SectionOrder: rank[e.section] + 1, DMUserID: dm,
			}
			if e.section == sectionInvites {
				item.Invite, item.InviteSpace = true, r.row.IsSpace
				if r.inviter != "" {
					item.Inviter = displayName(r, m.global, r.inviter)
				}
				ws.Channels = append(ws.Channels, item)
				continue
			}
			ws.Channels = append(ws.Channels, item)
			ws.Finder = append(ws.Finder, core.ChannelFinderItem{
				ID: r.alias, Name: r.row.Name, Type: string(r.row.Kind), Joined: true, DMUserID: dm,
			})
		}
	}

	out := []Workspace{{ID: "", Name: HomeName}}
	build(&out[0], home, []sidebar.SectionMeta{
		{ID: sectionDMs, Name: "Direct Messages", Type: "direct_messages"},
	})
	own := m.self.Homeserver()
	servers := slices.Collect(maps.Keys(byServer))
	slices.SortFunc(servers, func(a, b string) int {
		switch {
		case a == b:
			return 0
		case a == own, b == "":
			return -1
		case b == own, a == "":
			return 1
		}
		return cmpString(a, b)
	})
	for _, srv := range servers {
		name := srv
		if srv == "" {
			name = "Other rooms"
		}
		ws := Workspace{ID: "@" + srv, Name: name}
		build(&ws, byServer[srv], []sidebar.SectionMeta{{ID: sectionRooms, Name: "Rooms", Type: "channels"}})
		out = append(out, ws)
	}
	for _, top := range topList {
		entries := byTop[top]
		subs := map[string]*room{}
		for _, e := range entries {
			if e.section != sectionRooms && e.section != sectionInvites {
				subs[e.section] = m.rooms[m.byAlias[e.section]]
			}
		}
		subIDs := make([]string, 0, len(subs))
		for sec := range subs {
			subIDs = append(subIDs, sec)
		}
		slices.SortFunc(subIDs, func(a, b string) int {
			if na, nb := text.Fold(subs[a].row.Name), text.Fold(subs[b].row.Name); na != nb {
				return cmpString(na, nb)
			}
			return cmpString(a, b)
		})
		var sections []sidebar.SectionMeta
		for _, sec := range subIDs {
			sections = append(sections, sidebar.SectionMeta{ID: sec, Name: subs[sec].row.Name, Type: "standard"})
		}
		sections = append(sections, sidebar.SectionMeta{ID: sectionRooms, Name: "Rooms", Type: "channels"})
		ws := Workspace{ID: top.alias, Name: top.row.Name}
		build(&ws, entries, sections)
		out = append(out, ws)
	}
	return out
}

// serverOf is the server a room in no space is listed under: the one in
// its canonical alias (#name:server, the address its people chose, which
// may differ from the server it was created on); for a chat with neither
// a name nor an address (named after its members), own (the account's
// server) when one of its members is on it; else the one in its ID (room
// IDs from room version 12 on name none); else "".
func serverOf(r *room, own string) string {
	if _, srv, ok := strings.Cut(string(r.row.CanonicalAlias), ":"); ok && srv != "" {
		return srv
	}
	if r.row.CanonicalAlias == "" && (r.explicitName == nil || *r.explicitName == "") &&
		slices.ContainsFunc(r.heroes, func(u id.UserID) bool { return u.Homeserver() == own }) {
		return own
	}
	if _, srv, ok := strings.Cut(string(r.row.ID), ":"); ok && srv != "" {
		return srv
	}
	return ""
}
