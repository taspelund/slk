package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/ui/help"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// matrixApp is an App on C1/T1 showing three messages, driven by a
// Matrix backend, with clipboard writes recorded.
func matrixApp(t *testing.T, opts ...testOpt) (*App, *string) {
	t.Helper()
	a, copied := copyHuddleApp(t, append([]testOpt{withMessages(testMessageItems(3)...)}, opts...)...)
	a.SetBackend(Backend{Name: "Matrix"})
	keepToasts(t)
	return a, copied
}

// keepToasts makes toasts stay up for the test: their clear tick never
// fires, so a drained command cannot wipe the toast under test.
func keepToasts(t *testing.T) {
	old := toastTick
	toastTick = func(time.Duration, func(time.Time) tea.Msg) tea.Cmd { return nil }
	t.Cleanup(func() { toastTick = old })
}

func TestMatrixRefusesSlackOnlyFeatures(t *testing.T) {
	cases := []struct {
		name  string
		run   func(a *App) tea.Cmd
		toast string
	}{
		{"H", func(a *App) tea.Cmd { _, c := a.Update(keyPress('H')); return c }, "Matrix has no huddles"},
		{":huddle", func(a *App) tea.Cmd { return executeCommand(a, "huddle") }, "Matrix has no huddles"},
		{"F", func(a *App) tea.Cmd { _, c := a.Update(keyPress('F')); return c }, "Matrix has no message forwarding"},
		{"ctrl+a", func(a *App) tea.Cmd { _, c := a.Update(keyMod('a', tea.ModCtrl)); return c }, "Matrix has no Activity feed"},
		{":activity", func(a *App) tea.Cmd { return executeCommand(a, "activity") }, "Matrix has no Activity feed"},
		{"ctrl+f", func(a *App) tea.Cmd { _, c := a.Update(keyMod('f', tea.ModCtrl)); return c }, "Matrix has no workspace search"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, copied := matrixApp(t)
			focusMessages(t, a)
			for _, m := range drainCmd(c.run(a)) {
				a.Update(m)
			}
			if got := statusbarText(a); !strings.Contains(got, c.toast) {
				t.Errorf("status bar = %q, want %q", got, c.toast)
			}
			if *copied != "" || a.mode != ModeNormal || a.view != ViewChannels || a.pendingForward != nil {
				t.Errorf("acted anyway: clipboard %q, mode %v, view %v, forward %v", *copied, a.mode, a.view, a.pendingForward)
			}
		})
	}
}

func TestSlackKeepsSlackOnlyFeatures(t *testing.T) {
	a, copied := copyHuddleApp(t, withMessages(testMessageItems(3)...))
	focusMessages(t, a)
	_, cmd := a.Update(keyPress('H'))
	drainHuddleBatch(t, cmd)
	if *copied == "" {
		t.Error("H copied nothing on Slack")
	}
	if _, cmd := a.Update(keyMod('f', tea.ModCtrl)); cmd != nil || a.mode != ModeWorkspaceSearch {
		t.Errorf("ctrl+f on Slack: mode %v", a.mode)
	}
}

func TestMatrixSlashCommands(t *testing.T) {
	a, statuses := presenceSlashApp(t, "away")
	a.SetBackend(Backend{Name: "Matrix"})
	keepToasts(t)
	runs := &[]runCall{}
	a.setCommandRunnerForTest(func(team string, ch ids.ChannelID, th ids.ThreadTS, name, args string) error {
		*runs = append(*runs, runCall{team, string(ch), string(th), name, args})
		return nil
	})

	a.compose.SetValue("/giphy cats")
	_ = pressEnter(a)
	if len(*runs) != 0 {
		t.Fatalf("a server command ran: %+v", *runs)
	}
	if got := statusbarText(a); !strings.Contains(got, "/giphy: Matrix has no server slash commands") {
		t.Errorf("status bar = %q", got)
	}
	if got := draftIn(a, "C1"); got != "/giphy cats" {
		t.Errorf("draft = %q, want the command given back", got)
	}

	// slk's own commands still run.
	a.SetMode(ModeInsert)
	a.compose.SetValue("/active")
	_ = pressEnter(a)
	if len(*statuses) != 1 || (*statuses)[0].action != core.PresenceSetActive {
		t.Errorf("/active: status calls %+v", *statuses)
	}
}

func TestMatrixHelpLeavesOutSlackOnly(t *testing.T) {
	slackOnly := []string{"copy huddle link", "forward message", "open activity", "Activity: toggle unread-only",
		"search workspace", "remove link preview", "respond to message buttons"}
	descs := func(a *App) string {
		var b strings.Builder
		for _, e := range help.FromKeyMap(a.helpKeyMap()) {
			b.WriteString(e.Desc + "\n")
		}
		return b.String()
	}
	slack, _ := copyHuddleApp(t)
	matrix, _ := matrixApp(t)
	for _, d := range slackOnly {
		if !strings.Contains(descs(slack), d) {
			t.Errorf("Slack help lacks %q", d)
		}
		if strings.Contains(descs(matrix), d) {
			t.Errorf("Matrix help lists %q", d)
		}
	}
	if !strings.Contains(descs(matrix), "copy permalink") {
		t.Error("Matrix help lost an ordinary entry")
	}
	matrix.openHelpOverlay()
	if strings.Contains(stripANSI(matrix.help.ViewOverlay(120, 40, "")), "huddle") {
		t.Error("the open help overlay mentions huddles")
	}
}

func TestMatrixSidebarHasNoActivityRow(t *testing.T) {
	a, _ := matrixApp(t, withChannels(sidebar.ChannelItem{ID: "C1", Name: "general", Type: "channel"}))
	if v := stripANSI(a.sidebar.View(20, 30)); strings.Contains(v, "Activity") || !strings.Contains(v, "Threads") {
		t.Errorf("sidebar:\n%s", v)
	}
	a.sidebar.SelectActivityRow()
	if a.sidebar.IsActivitySelected() {
		t.Error("the hidden Activity row is selectable")
	}
}

func TestMatrixPresenceMenuOffersPresenceOnly(t *testing.T) {
	a, _ := matrixApp(t)
	a.Update(keyMod('s', tea.ModCtrl))
	if a.mode != ModePresenceMenu {
		t.Fatalf("mode = %v", a.mode)
	}
	v := stripANSI(a.presenceMenu.ViewOverlay(100, 30, ""))
	for _, want := range []string{"Active", "Away"} {
		if !strings.Contains(v, want) {
			t.Errorf("menu lacks %q:\n%s", want, v)
		}
	}
	for _, gone := range []string{"Snooze", "Working from home", "vacation"} {
		if strings.Contains(v, gone) {
			t.Errorf("menu offers %q:\n%s", gone, v)
		}
	}
}

// fakeLinks resolves the links in its map.
type fakeLinks map[string]core.LinkTarget

func (f fakeLinks) ResolveLink(url string) (core.LinkTarget, bool) {
	t, ok := f[url]
	return t, ok
}

func TestBackendLinksOpenInApp(t *testing.T) {
	app, opened := linkTestApp(t)
	const room, reply, other = "https://matrix.to/#/!r:x", "https://matrix.to/#/!r:x/$e", "https://matrix.to/#/!gone:x"
	app.SetLinkResolver(fakeLinks{
		room:  {ChannelID: "C054JFCBN69"},
		reply: {ChannelID: "C054JFCBN69", MessageTS: "2.000000", ThreadTS: "1.000000"},
		// The resolver knows it but the channel service does not.
		other: {ChannelID: "Cgone"},
	})
	if !app.linkOpensInApp(room) || !app.linkOpensInApp(reply) || app.linkOpensInApp(other) || app.linkOpensInApp("https://matrix.to/#/!x:y") {
		t.Error("linkOpensInApp disagrees with the resolver")
	}

	_, cmd := app.Update(OpenLinkMsg{URL: reply})
	msgs := drainCmd(cmd)
	if len(msgs) != 1 {
		t.Fatalf("got %#v, want one ChannelSelectedMsg", msgs)
	}
	if sel, ok := msgs[0].(ChannelSelectedMsg); !ok || sel.ID != "C054JFCBN69" || sel.TeamID != "T1" {
		t.Fatalf("got %#v", msgs[0])
	}
	if nav := app.pendingLinkNav; nav == nil || nav.MessageTS != "2.000000" || nav.ThreadTS != "1.000000" {
		t.Fatalf("pending nav = %+v", app.pendingLinkNav)
	}
	if *opened != "" {
		t.Errorf("browser opened %q", *opened)
	}

	for _, u := range []string{other, "https://matrix.to/#/!x:y"} {
		*opened = ""
		_, cmd = app.Update(OpenLinkMsg{URL: u})
		drainCmd(cmd)
		if *opened != u {
			t.Errorf("%s: browser opened %q", u, *opened)
		}
	}
}

// inviteApp is a Matrix App in T1 whose sidebar holds general (C1) and
// an invite to party (C7) from Carol, with joins and declines recorded
// and answered with joinErr / declineErr.
func inviteApp(t *testing.T, joinErr, declineErr error) (*App, *[]string) {
	t.Helper()
	calls := &[]string{}
	a := newTestApp(t, withActiveTeam("T1"), withActiveChannel("C1"),
		withChannels(
			sidebar.ChannelItem{ID: "C1", Name: "general", Type: "channel", Section: "rooms"},
			sidebar.ChannelItem{ID: "C7", Name: "party", Type: "private", Section: "invites", Invite: true, Inviter: "Carol"},
		),
		withChannelService(core.ChannelServiceFuncs{
			Lookup: func(ch ids.ChannelID) (string, string, bool) { return string(ch), "channel", true },
			Join: func(ch ids.ChannelID, name string) core.Msg {
				*calls = append(*calls, "join "+string(ch))
				if joinErr != nil {
					return ChannelJoinFailedMsg{ID: string(ch), Name: name, Err: joinErr}
				}
				return ChannelJoinedMsg{ID: string(ch), Name: "Party!"}
			},
			DeclineInvite: func(ch ids.ChannelID) core.Msg {
				*calls = append(*calls, "decline "+string(ch))
				return InviteDeclinedMsg{ChannelID: string(ch), Err: declineErr}
			},
		}))
	a.SetBackend(Backend{Name: "Matrix"})
	keepToasts(t)
	return a, calls
}

// selectInvite selects C7 the way the sidebar does and returns what the
// prompt's key k then delivers, fed back through Update.
func selectInvite(t *testing.T, a *App, k tea.KeyMsg) []tea.Msg {
	t.Helper()
	a.Update(ChannelSelectedMsg{ID: "C7", Name: "party", Type: "private"})
	if a.mode != ModeConfirm || !dialogShows(a, "Carol invited you to party.") {
		t.Fatalf("mode %v; the invite prompt did not open:\n%s", a.mode, a.confirmPrompt.View())
	}
	if a.activeChannelID != "C1" {
		t.Fatalf("the invite was opened: active channel %q", a.activeChannelID)
	}
	return pressKey(t, a, k)
}

func TestInviteJoin(t *testing.T) {
	a, calls := inviteApp(t, nil, nil)
	msgs := selectInvite(t, a, keyPress('j'))
	if len(*calls) != 1 || (*calls)[0] != "join C7" {
		t.Fatalf("calls = %v", *calls)
	}
	var opened bool
	for _, m := range msgs {
		if sel, ok := m.(ChannelSelectedMsg); ok && sel.ID == "C7" && sel.Type == "private" {
			opened = true
			a.Update(sel)
		}
	}
	if !opened || a.activeChannelID != "C7" {
		t.Errorf("joined invite not opened: active %q, msgs %#v", a.activeChannelID, msgs)
	}
	if it, ok := a.inviteItem("C7"); ok || it.Name != "Party!" {
		t.Errorf("row after join = %+v, invite %v", it, ok)
	}
}

func TestInviteJoinFails(t *testing.T) {
	a, _ := inviteApp(t, errors.New("M_FORBIDDEN"), nil)
	selectInvite(t, a, keyPress('j'))
	if got := statusbarText(a); !strings.Contains(got, "Couldn't join party: M_FORBIDDEN") {
		t.Errorf("status bar = %q", got)
	}
	if _, ok := a.inviteItem("C7"); !ok || a.activeChannelID != "C1" {
		t.Errorf("a failed join changed the sidebar or opened the room")
	}
}

func TestInviteDecline(t *testing.T) {
	a, calls := inviteApp(t, nil, nil)
	selectInvite(t, a, keyPress('d'))
	if len(*calls) != 1 || (*calls)[0] != "decline C7" {
		t.Fatalf("calls = %v", *calls)
	}
	for _, it := range a.sidebar.Items() {
		if it.ID == "C7" {
			t.Errorf("declined invite still in the sidebar: %+v", it)
		}
	}
	if got := statusbarText(a); !strings.Contains(got, "Declined the invite to party") {
		t.Errorf("status bar = %q", got)
	}

	a, _ = inviteApp(t, nil, errors.New("offline"))
	selectInvite(t, a, keyPress('d'))
	if _, ok := a.inviteItem("C7"); !ok {
		t.Error("a failed decline dropped the row")
	}
	if got := statusbarText(a); !strings.Contains(got, "Couldn't decline the invite to party: offline") {
		t.Errorf("status bar = %q", got)
	}
}

func TestInviteCancelAndReadOnly(t *testing.T) {
	a, calls := inviteApp(t, nil, nil)
	selectInvite(t, a, keyCode(tea.KeyEscape))
	if len(*calls) != 0 || a.mode == ModeConfirm {
		t.Errorf("Esc: calls %v, mode %v", *calls, a.mode)
	}

	a, calls = inviteApp(t, nil, nil)
	a.SetBackend(Backend{Name: "Matrix", ReadOnly: true})
	a.Update(ChannelSelectedMsg{ID: "C7", Name: "party", Type: "private"})
	if a.mode == ModeConfirm || len(*calls) != 0 || a.activeChannelID != "C1" {
		t.Errorf("read-only: mode %v, calls %v, active %q", a.mode, *calls, a.activeChannelID)
	}
	if got := statusbarText(a); !strings.Contains(got, "Read-only session: invites can't be accepted") {
		t.Errorf("status bar = %q", got)
	}
}

func TestInvitesAreNotChannelPicks(t *testing.T) {
	a, _ := inviteApp(t, nil, nil)
	a.SetMode(ModeInsert)
	_ = a.compose.Focus()
	typeInCompose(a, "#")
	if v := stripANSI(a.compose.ChannelPickerView(80)); strings.Contains(v, "party") || !strings.Contains(v, "general") {
		t.Errorf("# picker:\n%s", v)
	}
}

func TestMatrixThreadsBannerDoesNotNameSlack(t *testing.T) {
	a, _ := matrixApp(t)
	a.threadsView.SetSubscriptionsAvailable(false)
	v := stripANSI(a.threadsView.View(10, 200))
	if strings.Contains(v, "Slack") || !strings.Contains(v, "Matrix's thread list could not be fetched") {
		t.Errorf("banner:\n%s", v)
	}
}
