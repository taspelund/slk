// internal/ui/invites.go
//
// Pending invites: sidebar rows with ChannelItem.Invite (a Matrix
// backend's "Invites" section). Selecting one asks Join / Decline /
// Cancel instead of opening it; Join goes through ChannelService.Join,
// Decline through ChannelService.DeclineInvite. The backend's live
// updates then move a joined room to its own section and drop a
// declined one; the reducer (reducer_invites.go) only bridges the time
// until they arrive.
package ui

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/bubbles/confirmprompt"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// InviteDeclinedMsg is ChannelService.DeclineInvite's answer: the
// invite to ChannelID was declined, or Err says why not.
type InviteDeclinedMsg struct {
	ChannelID string
	Err       error
}

// inviteAnsweredMsg is a Join or Decline pressed in the invite prompt,
// with the service's answer (result: ChannelJoinedMsg or
// ChannelJoinFailedMsg for a join, InviteDeclinedMsg for a decline; nil
// when the service did not answer). teamID is the workspace the prompt
// was opened in.
type inviteAnsweredMsg struct {
	teamID string
	item   sidebar.ChannelItem
	join   bool
	result tea.Msg
}

// inviteItem returns the sidebar's invite row with id, if it is one.
func (a *App) inviteItem(id string) (sidebar.ChannelItem, bool) {
	for _, it := range a.sidebar.Items() {
		if it.ID == id {
			return it, it.Invite
		}
	}
	return sidebar.ChannelItem{}, false
}

// inviteText is the prompt's body for an invite.
func inviteText(item sidebar.ChannelItem) string {
	direct := item.Type == "dm" || item.Type == "group_dm"
	switch {
	case direct && item.Inviter != "":
		return item.Inviter + " invited you to a direct chat."
	case direct:
		return "You're invited to a direct chat with " + item.Name + "."
	}
	what := item.Name
	if item.InviteSpace {
		what = "the space " + item.Name
	}
	if item.Inviter != "" {
		return item.Inviter + " invited you to " + what + "."
	}
	return "You're invited to " + what + "."
}

// openInvitePrompt asks whether to join or decline the invite item. A
// read-only session can do neither, and says so instead.
func (a *App) openInvitePrompt(item sidebar.ChannelItem) tea.Cmd {
	if a.backend.ReadOnly {
		return toastWithClear(a, "Read-only session: invites can't be accepted or declined", 3*time.Second)
	}
	channels, team := a.channels, a.activeTeamID
	id := ids.ChannelID(item.ID)
	a.openConfirmChoice(confirmprompt.Choice{
		Title: "Invitation",
		Body:  inviteText(item),
		Buttons: []confirmprompt.Button{
			{Label: "Join", Keys: key.NewBinding(key.WithKeys("j")), Action: func() tea.Msg {
				return inviteAnsweredMsg{teamID: team, item: item, join: true, result: channels.Join(id, item.Name)}
			}},
			{Label: "Decline", Keys: key.NewBinding(key.WithKeys("d")), Action: func() tea.Msg {
				return inviteAnsweredMsg{teamID: team, item: item, result: channels.DeclineInvite(id)}
			}},
			{Label: "Cancel", Hint: "Esc"},
		},
	})
	return nil
}
