package ui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/core"

	"github.com/gammons/slk/internal/ui/sidebar"
)

// staticSections is a sidebar.SectionsProvider over a fixed list, the
// shape the Matrix backend's workspaces hand the sidebar.
type staticSections []sidebar.SectionMeta

func (s staticSections) Ready() bool                                 { return true }
func (s staticSections) OrderedSlackSections() []sidebar.SectionMeta { return s }

// matrixGoldenApp is the golden fixture as a Matrix workspace: an
// Invites section (a room invite with its inviter, one without, a space
// invite) above
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
		{ID: "C9", Name: "Oxide", Type: "private", Section: "invites", Invite: true, Inviter: "Dana", InviteSpace: true},
		{ID: "C1", Name: "general", Type: "channel", Section: "rooms"},
		{ID: "C2", Name: "engineering", Type: "channel", Section: "rooms"},
	})
	return a
}

// goldenReaderNames are display names for the golden fixture's users
// and for readers U5..U16, for the read-marker goldens.
func goldenReaderNames() map[string]string {
	names := map[string]string{"U1": "alice", "U2": "bob", "U3": "carol", "U4": "dave"}
	for i := 5; i <= 16; i++ {
		names[fmt.Sprintf("U%d", i)] = fmt.Sprintf("reader-%d", i)
	}
	return names
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
		}, []string{"Invites", "party · Carol", "book-club", "Oxide (space) · Dana", "Rooms", "Matrix has no huddles"}},
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
		{"matrix_read_markers", func(a *App) {
			a.SetUserNames(goldenReaderNames())
			msgs := goldenMessages()
			msgs[0].SeenBy = []string{"U4"}
			msgs[1].SeenBy = []string{"U1", "U3"}
			last := len(msgs) - 1
			msgs[last].SeenBy = []string{"U2", "U3", "U4"}
			for i := 5; i <= 16; i++ {
				msgs[last].SeenBy = append(msgs[last].SeenBy, fmt.Sprintf("U%d", i))
			}
			a.messagepane.SetMessages(msgs)
		}, []string{"Seen by dave", "Seen by alice, carol", "Seen by bob, carol, dave, reader-5, reader-6", "+"}},
		{"matrix_seen_dialog", func(a *App) {
			a.Update(seenLoadedMsg{readers: []core.Reader{
				{UserID: "U2", Name: "bob", ReadTS: goldenTS(5 * time.Minute), ReadTime: "9:05 AM"},
				{UserID: "U3", Name: "carol", ReadTS: goldenTS(-23 * time.Hour), ReadTime: "10:00 AM"},
				{UserID: "U4", Name: "dave"},
			}})
		}, []string{"Read receipts", "Seen by  (3)", "bob · ", "9:05 AM", "carol · ", "10:00 AM", "dave"}},
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
