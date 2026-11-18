package rooms

import (
	"slices"

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
// The first is Home (ID ""): direct chats, in section "Direct Messages",
// and joined rooms in no space, in "Rooms". Then each top-level space (a
// joined space with no joined parent space), by name, then alias, with ID
// its room alias: the rooms under it, at any depth, each in the section
// of its nearest parent space below the top (a sub-space's name, sorted
// by name) or, when that parent is the top itself, in "Rooms" after them.
// A room under two top-level spaces is in both. Rooms whose only spaces
// form a cycle with no top are in Home.
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
		if !r.sidebar() {
			continue
		}
		if r.row.Kind == store.RoomDM || r.row.Kind == store.RoomGroupDM {
			home = append(home, entry{r, sectionDMs})
			continue
		}
		ts := tops(r.row.ID, map[id.RoomID]bool{})
		if len(ts) == 0 {
			home = append(home, entry{r, sectionRooms})
			continue
		}
		ps := parents(r.row.ID)
		for top := range ts {
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
			ws.Channels = append(ws.Channels, sidebar.ChannelItem{
				ID: r.alias, Name: r.row.Name, Type: string(r.row.Kind), Section: e.section,
				SectionOrder: rank[e.section] + 1, DMUserID: dm,
			})
			ws.Finder = append(ws.Finder, core.ChannelFinderItem{
				ID: r.alias, Name: r.row.Name, Type: string(r.row.Kind), Joined: true, DMUserID: dm,
			})
		}
	}

	out := []Workspace{{ID: "", Name: HomeName}}
	build(&out[0], home, []sidebar.SectionMeta{
		{ID: sectionDMs, Name: "Direct Messages", Type: "direct_messages"},
		{ID: sectionRooms, Name: "Rooms", Type: "channels"},
	})
	for _, top := range topList {
		entries := byTop[top]
		subs := map[string]*room{}
		for _, e := range entries {
			if e.section != sectionRooms {
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
