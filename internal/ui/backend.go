// internal/ui/backend.go
//
// Which backend drives the App, for the features only Slack has.
package ui

import (
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/core"
)

// Backend names the backend behind the App when it is not Slack. One
// backend drives a process, so this is App-wide, set once before the
// program starts (SetBackend), not per workspace.
//
// The zero value is Slack, which has everything. Any other backend
// (Name set) lacks Slack's own features, and the App hides or refuses
// them rather than letting them fail: huddles (H / :huddle), server slash
// commands (slk's built-ins still run), forwarding (F), the Activity feed
// (its sidebar row, ctrl+a, :activity), workspace search (ctrl+f), and
// custom status and snooze in the presence menu (ctrl+s offers Active and
// Away). The help overlay leaves them out, with link-preview removal (x)
// and message buttons (b), which a non-Slack message never offers.
type Backend struct {
	// Name is the backend as refusals name it ("Matrix"); "" is Slack.
	Name string
	// ReadOnly says the session does not change the account, so an
	// invite cannot be accepted or declined.
	ReadOnly bool
}

// slack reports whether the backend is Slack.
func (b Backend) slack() bool { return b.Name == "" }

// SetBackend tells the App which backend drives it. Call it before the
// program starts.
func (a *App) SetBackend(b Backend) {
	a.backend = b
	a.sidebar.SetActivityRowHidden(!b.slack())
	a.presenceMenu.SetPresenceOnly(!b.slack())
	if !b.slack() {
		a.threadsView.SetUnavailableText("Threads list unavailable — " + b.Name +
			"'s thread list could not be fetched. slk will try again in a few minutes.")
	}
}

// SetLinkResolver wires the backend's own links (see core.LinkResolver);
// nil leaves only Slack permalinks opening in the app.
func (a *App) SetLinkResolver(r core.LinkResolver) {
	a.linkResolver = r
}

// resolveBackendLink asks the backend's LinkResolver where rawURL
// points; ok is false without one.
func (a *App) resolveBackendLink(rawURL string) (core.LinkTarget, bool) {
	if a.linkResolver == nil {
		return core.LinkTarget{}, false
	}
	return a.linkResolver.ResolveLink(rawURL)
}

// refuseSlackOnly refuses a Slack-only feature the backend lacks with a
// toast, e.g. "Matrix has no huddles" for what "huddles". refused is
// false on Slack, which goes on.
func (a *App) refuseSlackOnly(what string) (cmd tea.Cmd, refused bool) {
	if a.backend.slack() {
		return nil, false
	}
	return toastWithClear(a, a.backend.Name+" has no "+what, 3*time.Second), true
}

// helpKeyMap is the key map the help overlay lists: a.keys without
// what the backend lacks (see Backend), and on Slack without :seen
// (Slack has no read receipts).
func (a *App) helpKeyMap() KeyMap {
	km := a.keys
	if a.backend.slack() {
		km.SeenBy.SetHelp("", "")
		return km
	}
	for _, b := range []*key.Binding{
		&km.Huddle, &km.ForwardMessage, &km.ActivityView, &km.ActivityUnread,
		&km.WorkspaceSearch, &km.RemovePreview, &km.RespondButtons,
	} {
		b.SetHelp("", "")
	}
	return km
}
