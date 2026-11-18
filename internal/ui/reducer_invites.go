// internal/ui/reducer_invites.go
//
// Invite reducer for App.Update (invites.go):
//
//	inviteAnsweredMsg - Join or Decline answered. A join makes the row
//	                    an ordinary one and opens it; a decline drops
//	                    the row. Either way the backend's live updates
//	                    follow with the authoritative sidebar. A failure
//	                    is a toast.
package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/ui/sidebar"
)

var reduceInvites reducerFunc = func(a *App, msg tea.Msg) (tea.Cmd, bool) {
	m, ok := msg.(inviteAnsweredMsg)
	if !ok {
		return nil, false
	}
	here := m.teamID == a.activeTeamID
	item := m.item
	if m.join {
		switch r := m.result.(type) {
		case ChannelJoinedMsg:
			if !here {
				return nil, true
			}
			if r.Name != "" {
				item.Name = r.Name
			}
			// Until the backend moves it, the row stays where it was,
			// as a conversation: selecting it again must open it, not
			// ask again.
			if _, still := a.inviteItem(item.ID); still {
				item.Invite = false
				a.sidebar.UpsertItem(item)
				a.SetChannels(a.sidebar.Items())
			}
			a.sidebar.SelectByID(item.ID)
			sel := ChannelSelectedMsg{ID: item.ID, Name: item.Name, Type: item.Type, TeamID: m.teamID}
			return func() tea.Msg { return sel }, true
		case ChannelJoinFailedMsg:
			return toastWithClear(a, "Couldn't join "+item.Name+": "+errText(r.Err), 4*time.Second), true
		}
		return toastWithClear(a, "Couldn't join "+item.Name, 4*time.Second), true
	}
	r, ok := m.result.(InviteDeclinedMsg)
	if !ok {
		return toastWithClear(a, "Couldn't decline the invite to "+item.Name, 4*time.Second), true
	}
	if r.Err != nil {
		return toastWithClear(a, "Couldn't decline the invite to "+item.Name+": "+errText(r.Err), 4*time.Second), true
	}
	if here {
		if _, still := a.inviteItem(item.ID); still {
			var kept []sidebar.ChannelItem
			for _, it := range a.sidebar.Items() {
				if it.ID != item.ID {
					kept = append(kept, it)
				}
			}
			a.SetChannels(kept)
		}
	}
	return toastWithClear(a, "Declined the invite to "+item.Name, 3*time.Second), true
}

// errText is err's message, "unknown error" for nil.
func errText(err error) string {
	if err == nil {
		return "unknown error"
	}
	return err.Error()
}
