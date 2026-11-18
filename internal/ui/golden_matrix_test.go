package ui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/ui/sidebar"
)

// staticSections is a sidebar.SectionsProvider over a fixed list, the
// shape the Matrix backend's workspaces hand the sidebar.
type staticSections []sidebar.SectionMeta

func (s staticSections) Ready() bool                                 { return true }
func (s staticSections) OrderedSlackSections() []sidebar.SectionMeta { return s }

// matrixGoldenApp is the golden fixture as a Matrix workspace: an
// Invites section (a room invite with its inviter, one without) above
// Rooms, and no Activity row.
func matrixGoldenApp(t *testing.T) *App {
	t.Helper()
	a := newGoldenApp(t, withSize(goldenBaseW, goldenBaseH), withActiveChannel("C1"), withMessages(goldenMessages()...))
	a.SetBackend(Backend{Name: "Matrix"})
	a.sidebar.SetSectionsProvider(staticSections{
		{ID: "invites", Name: "Invites", Type: "invites"},
		{ID: "rooms", Name: "Rooms", Type: "channels"},
	})
	a.SetChannels([]sidebar.ChannelItem{
		{ID: "C7", Name: "party", Type: "private", Section: "invites", Invite: true, Inviter: "Carol"},
		{ID: "C8", Name: "book-club", Type: "channel", Section: "invites", Invite: true},
		{ID: "C1", Name: "general", Type: "channel", Section: "rooms"},
		{ID: "C2", Name: "engineering", Type: "channel", Section: "rooms"},
	})
	return a
}

func TestGolden_Matrix(t *testing.T) {
	cases := []struct {
		name string
		act  func(a *App)
		want []string
	}{
		{"matrix_invites_huddle_refused", func(a *App) {
			a.focusedPanel = PanelMessages
			a.Update(keyPress('H'))
		}, []string{"Invites", "party · Carol", "book-club", "Rooms", "Matrix has no huddles"}},
		{"matrix_invite_prompt", func(a *App) {
			a.Update(ChannelSelectedMsg{ID: "C7", Name: "party", Type: "private"})
		}, []string{"Invitation", "Carol invited you to party.", "Join", "Decline", "Cancel"}},
		{"matrix_slash_refused", func(a *App) {
			a.focusedPanel = PanelMessages
			a.activeTeamID = "T1"
			a.compose.SetDraftContext("T1", "C1", "")
			a.SetMode(ModeInsert)
			_ = a.compose.Focus()
			a.compose.SetValue("/giphy cats")
			a.Update(keyCode(tea.KeyEnter))
		}, []string{"/giphy: Matrix has no server slash commands", "/giphy cats"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := matrixGoldenApp(t)
			c.act(a)
			frame := a.View().Content
			plain := stripANSI(frame)
			for _, w := range c.want {
				if !strings.Contains(plain, w) {
					t.Errorf("%q is not on screen:\n%s", w, plain)
				}
			}
			if strings.Contains(plain, "Activity") {
				t.Errorf("the Matrix sidebar shows Activity:\n%s", plain)
			}
			if lines := strings.Count(frame, "\n") + 1; lines != goldenBaseH {
				t.Errorf("frame has %d lines, want %d", lines, goldenBaseH)
			}
			compareGolden(t, c.name, frame)
		})
	}
}
