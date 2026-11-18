package svc

import (
	"slices"
	"sync"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/sidebar"
	"github.com/gammons/slk/internal/ui/workspace"
)

// The account's rooms are several UI workspaces, one per rail entry
// (rooms.Workspace): Direct Messages (DMs only), then one per server for
// rooms in no space, then one per top-level space. Each has its own team
// ID: the backend's TeamID for Direct Messages, "M@" plus the server for
// a server's, "M" plus the space's room alias for a space.
//
// The UI shows one at a time. As for Slack's inactive workspaces, a room
// that is not in the active one sends no message, reaction or typing
// events, only ReadStateChangedMsg for its workspaces so the rail's dot
// follows; opening it later loads it from the store. The exception is the
// room the UI opened (fetch) since the last switch, such as a DM started
// from a server's workspace: it is on screen wherever it belongs. A room
// in two spaces is in both workspaces.

// workspaces is the backend's workspace state (Backend.ws). Lookups read
// the room model; list is the workspaces as last told to the UI
// (refreshWorkspaces), to tell what changed.
type workspaces struct {
	mu   sync.Mutex
	list []rooms.Workspace
	// sections are each workspace's sidebar sections provider, by
	// team ID, kept across rebuilds so the UI's reference stays live.
	sections map[string]*sectionsProvider
	active   string
	// open is the room (by alias) the UI last fetched since the last
	// switch: it can be outside the workspace on screen, as a DM opened
	// from a server's workspace is, and its events still reach the UI.
	open string
}

// teamOf is the team ID of workspace wsID ("" is Direct Messages).
func (b *Backend) teamOf(wsID string) string {
	if wsID == "" {
		return b.teamID
	}
	return "M" + wsID
}

// refreshWorkspaces rebuilds the workspaces from the room model. It
// reports whether the rail changed (workspaces added, removed, renamed or
// reordered), and which workspaces' sidebars changed.
func (b *Backend) refreshWorkspaces() (railChanged bool, changed []rooms.Workspace) {
	list, teams := b.current()
	w := &b.ws
	w.mu.Lock()
	defer w.mu.Unlock()
	old := map[string]rooms.Workspace{}
	for _, ws := range w.list {
		old[ws.ID] = ws
	}
	railChanged = len(list) != len(w.list)
	for i, ws := range list {
		if !railChanged && (w.list[i].ID != ws.ID || w.list[i].Name != ws.Name) {
			railChanged = true
		}
		prev, ok := old[ws.ID]
		if !ok || !slices.Equal(prev.Channels, ws.Channels) || !slices.Equal(prev.Sections, ws.Sections) {
			changed = append(changed, ws)
		}
		p := w.sections[teams[i]]
		if p == nil {
			p = &sectionsProvider{}
			w.sections[teams[i]] = p
		}
		p.set(ws.Sections)
	}
	w.list = list
	if !slices.Contains(teams, w.active) {
		w.active = b.teamID
	}
	return railChanged, changed
}

// current is the workspaces as the room model has them now, with their
// team IDs.
func (b *Backend) current() ([]rooms.Workspace, []string) {
	list := b.rooms.Workspaces()
	teams := make([]string, len(list))
	for i, ws := range list {
		teams[i] = b.teamOf(ws.ID)
	}
	return list, teams
}

// railItems are the workspaces as rail items, in order.
func (b *Backend) railItems() []workspace.WorkspaceItem {
	list, teams := b.current()
	out := make([]workspace.WorkspaceItem, len(list))
	for i, ws := range list {
		out[i] = workspace.WorkspaceItem{ID: teams[i], Name: ws.Name, Initials: workspace.WorkspaceInitials(ws.Name)}
	}
	return out
}

// workspace returns the workspace a team ID names, with its sections
// provider.
func (b *Backend) workspace(team string) (rooms.Workspace, *sectionsProvider, bool) {
	list, teams := b.current()
	i := slices.Index(teams, team)
	if i < 0 {
		return rooms.Workspace{}, nil, false
	}
	w := &b.ws
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.sections[team]
	if p == nil {
		p = &sectionsProvider{}
		p.set(list[i].Sections)
		w.sections[team] = p
	}
	return list[i], p, true
}

// teamsOf are the team IDs of the workspaces a room (by alias) is in.
func (b *Backend) teamsOf(alias string) []string {
	list, teams := b.current()
	var out []string
	for i, ws := range list {
		if slices.ContainsFunc(ws.Channels, func(c sidebar.ChannelItem) bool { return c.ID == alias }) {
			out = append(out, teams[i])
		}
	}
	return out
}

// activeTeam is the workspace on screen (Direct Messages before the UI
// has one).
func (b *Backend) activeTeam() string {
	b.ws.mu.Lock()
	defer b.ws.mu.Unlock()
	if b.ws.active == "" {
		return b.teamID
	}
	return b.ws.active
}

// inActive says whether a room (by alias) is on screen: in the workspace
// on screen, or the conversation the UI opened there.
func (b *Backend) inActive(alias string) bool {
	b.ws.mu.Lock()
	open := b.ws.open
	b.ws.mu.Unlock()
	return alias == open || slices.Contains(b.teamsOf(alias), b.activeTeam())
}

// opened records the conversation the UI has open (fetch).
func (b *Backend) opened(alias string) {
	b.ws.mu.Lock()
	b.ws.open = alias
	b.ws.mu.Unlock()
}

// knownTeam says whether team is one of the backend's workspaces.
func (b *Backend) knownTeam(team string) bool {
	_, _, ok := b.workspace(team)
	return ok
}

// switchTo is WorkspaceService.Switch: the workspace's sidebar as a
// WorkspaceSwitchedMsg, nil for a team ID that is not the backend's.
func (b *Backend) switchTo(team string) core.Msg {
	ws, sections, ok := b.workspace(team)
	if !ok {
		return nil
	}
	b.ws.mu.Lock()
	b.ws.active, b.ws.open = team, ""
	b.ws.mu.Unlock()
	snap := b.rooms.Snapshot()
	theme, width := b.style(team)
	return ui.WorkspaceSwitchedMsg{
		TeamID:           team,
		TeamName:         ws.Name,
		Theme:            theme,
		SidebarWidth:     width,
		Channels:         ws.Channels,
		ChannelNames:     snap.ChannelNames,
		FinderItems:      ws.Finder,
		UserNames:        snap.UserNames,
		UserID:           b.selfAlias(),
		SectionsProvider: sections,
	}
}

// style is the theme and sidebar width the config gives a workspace
// (Deps.Style), empty when it gives none.
func (b *Backend) style(team string) (string, int) {
	if b.d.Style == nil {
		return "", 0
	}
	return b.d.Style(team)
}
