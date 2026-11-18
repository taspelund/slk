// internal/ui/read_receipts.go
//
// Other users' read receipts, for a backend that has them (Matrix; Slack
// has none): the "Seen by" markers under messages, moved live by
// ReadMarkersMovedMsg, the [general] read_receipts switch that hides
// them, and :seen, which lists who has read the selected message.
package ui

import (
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ui/messages"
	"github.com/gammons/slk/internal/ui/reactionsview"
)

// SetReadReceiptService wires :seen. nil (Slack) refuses it.
func (a *App) SetReadReceiptService(s core.ReadReceiptService) {
	a.readReceipts = s
}

// SetReadMarkers shows or hides the read markers under messages
// ([general] read_receipts, default on). :seen works either way.
func (a *App) SetReadMarkers(show bool) {
	a.hideReadMarkers = !show
	for _, m := range a.allWinModels() {
		m.SetHideSeen(!show)
	}
	a.threadPanel.SetHideSeen(!show)
}

// seenLoadedMsg is ReadReceiptService.SeenBy's answer for :seen.
type seenLoadedMsg struct {
	readers []core.Reader
	err     error
}

var reduceReadReceipts reducerFunc = func(a *App, msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case ReadMarkersMovedMsg:
		a.moveReadMarkers(m)
		return nil, true
	case seenLoadedMsg:
		return a.openSeenBy(m), true
	}
	return nil, false
}

// moveReadMarkers applies m to every pane showing its timeline: each
// channel window on the channel, or the thread pane on the thread.
func (a *App) moveReadMarkers(m ReadMarkersMovedMsg) {
	var panes []contentPane
	if m.ThreadTS == "" {
		for _, mm := range a.modelsForChannel(m.ChannelID) {
			panes = append(panes, mm)
		}
	} else if a.threadPanel.ChannelID() == m.ChannelID && a.threadPanel.ThreadTS() == m.ThreadTS {
		panes = append(panes, a.threadPanel)
	}
	for _, p := range panes {
		for _, mv := range m.Moves {
			p.MoveSeen(mv.UserID, mv.TS)
		}
	}
}

// cmdSeen is :seen: who has read the selected message in the focused
// pane (ReadReceiptService.SeenBy, off the UI goroutine), in a dialog.
func cmdSeen(a *App, _ commandArgs) tea.Cmd {
	svc := a.readReceipts
	if a.backend.slack() || svc == nil {
		return toastWithClear(a, "Slack has no read receipts", 3*time.Second)
	}
	channelID, ts, _, _, panel, ok := a.selectedMessageContext()
	if !ok {
		return toastWithClear(a, "No message selected", 2*time.Second)
	}
	threadTS := ""
	if panel == PanelThread && ts != a.threadPanel.ThreadTS() {
		threadTS = a.threadPanel.ThreadTS()
	}
	return func() tea.Msg {
		readers, err := svc.SeenBy(channelID, threadTS, ts)
		return seenLoadedMsg{readers: readers, err: err}
	}
}

// openSeenBy shows :seen's answer: the readers in the reactions dialog,
// each with when they read it if known.
func (a *App) openSeenBy(m seenLoadedMsg) tea.Cmd {
	if m.err != nil {
		return toastWithClear(a, "Could not read receipts: "+m.err.Error(), 3*time.Second)
	}
	if a.mode != ModeNormal {
		return nil // the user moved on (another dialog, typing) meanwhile
	}
	if len(m.readers) == 0 {
		return toastWithClear(a, "Nobody else has read this message yet", 3*time.Second)
	}
	rows := make([]string, len(m.readers))
	for i, r := range m.readers {
		rows[i] = r.Name
		if r.ReadTime != "" {
			rows[i] += " · " + messages.LongTimestamp(r.ReadTS, r.ReadTime)
		}
	}
	a.reactionsView.OpenTitled("Read receipts", []reactionsview.ReactionGroup{
		{Label: "Seen by", Users: rows, Count: len(rows)},
	})
	a.SetMode(ModeReactionsView)
	// The reactions list's mode, under the dialog's own name.
	a.statusbar.SetMode(modeLabel("SEEN"))
	return nil
}

// modeLabel is a status-bar mode label for a mode shown under another
// name.
type modeLabel string

func (l modeLabel) String() string { return string(l) }
