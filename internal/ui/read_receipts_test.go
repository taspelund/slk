package ui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ui/help"
	"github.com/gammons/slk/internal/ui/messages"
)

// fakeReceipts is a core.ReadReceiptService answering readers, recording
// what it was asked.
type fakeReceipts struct {
	readers []core.Reader
	asked   []string // "channel thread ts"
}

func (f *fakeReceipts) SeenBy(channelID, threadTS, ts string) ([]core.Reader, error) {
	f.asked = append(f.asked, channelID+" "+threadTS+" "+ts)
	return f.readers, nil
}

// seenOf is the SeenBy of each message (by TS) a pane holds.
func seenOf(msgs []messages.MessageItem) map[string][]string {
	out := map[string][]string{}
	for _, m := range msgs {
		if len(m.SeenBy) > 0 {
			out[m.TS] = m.SeenBy
		}
	}
	return out
}

// TestReadMarkersMovedMsgMovesMarkersInTheirTimeline: the channel's moves
// reach the channel pane, a thread's the thread pane on that thread only.
func TestReadMarkersMovedMsgMovesMarkersInTheirTimeline(t *testing.T) {
	msgs := testMessageItems(3)
	msgs[0].SeenBy = []string{"U2"}
	a, _ := matrixApp(t, withMessages(msgs...))
	focusThreadPanel(t, a)

	a.Update(ReadMarkersMovedMsg{ChannelID: "C1", Moves: []SeenMove{{UserID: "U2", TS: "3.0"}, {UserID: "U3", TS: "2.0"}}})
	if got, want := seenOf(a.messagepane.Messages()), map[string][]string{"3.0": {"U2"}, "2.0": {"U3"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("channel markers = %v, want %v", got, want)
	}
	if got := seenOf(a.threadPanel.Replies()); len(got) != 0 {
		t.Errorf("a channel move reached the thread: %v", got)
	}

	// Another thread, another channel: nothing.
	a.Update(ReadMarkersMovedMsg{ChannelID: "C1", ThreadTS: "99.0", Moves: []SeenMove{{UserID: "U3", TS: "11.0"}}})
	a.Update(ReadMarkersMovedMsg{ChannelID: "C2", ThreadTS: "10.0", Moves: []SeenMove{{UserID: "U3", TS: "11.0"}}})
	if got := seenOf(a.threadPanel.Replies()); len(got) != 0 {
		t.Errorf("another thread's move reached the thread: %v", got)
	}
	a.Update(ReadMarkersMovedMsg{ChannelID: "C1", ThreadTS: "10.0", Moves: []SeenMove{{UserID: "U3", TS: "11.0"}}})
	if got, want := seenOf(a.threadPanel.Replies()), map[string][]string{"11.0": {"U3"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("thread markers = %v, want %v", got, want)
	}
	if got := seenOf(a.messagepane.Messages()); !reflect.DeepEqual(got["2.0"], []string{"U3"}) {
		t.Errorf("a thread move reached the channel: %v", got)
	}
	// Moving on in the thread takes the old marker off.
	a.Update(ReadMarkersMovedMsg{ChannelID: "C1", ThreadTS: "10.0", Moves: []SeenMove{{UserID: "U3", TS: "12.0"}}})
	if got, want := seenOf(a.threadPanel.Replies()), map[string][]string{"12.0": {"U3"}}; !reflect.DeepEqual(got, want) {
		t.Errorf("thread markers after a move = %v, want %v", got, want)
	}
}

// TestThreadParentDrawsNoChannelMarkers: the parent copied from the
// channel carries the channel's markers; the thread pane drops them.
func TestThreadParentDrawsNoChannelMarkers(t *testing.T) {
	a, _ := matrixApp(t)
	a.threadPanel.SetThread(messages.MessageItem{TS: "10.0", UserID: "U1", Text: "parent", SeenBy: []string{"U2"}}, nil, "C1", "10.0")
	if got := a.threadPanel.ParentMsg().SeenBy; got != nil {
		t.Errorf("parent SeenBy = %v", got)
	}
}

// TestReadReceiptsOptionHidesMarkers: [general] read_receipts off hides
// every pane's markers, windows opened later included.
func TestReadReceiptsOptionHidesMarkers(t *testing.T) {
	msgs := testMessageItems(2)
	msgs[1].SeenBy = []string{"U2"}
	a, _ := matrixApp(t, withMessages(msgs...), withSize(100, 30))
	a.SetUserNames(map[string]string{"U1": "alice", "U2": "Bob"})
	if !strings.Contains(stripANSI(a.View().Content), "Seen by Bob") {
		t.Fatalf("marker not drawn:\n%s", stripANSI(a.View().Content))
	}
	a.SetReadMarkers(false)
	if strings.Contains(stripANSI(a.View().Content), "Seen by") {
		t.Fatalf("marker drawn with read_receipts off:\n%s", stripANSI(a.View().Content))
	}
	if m := a.newWindowModel("general"); m == nil {
		t.Fatal("no window model")
	} else {
		m.SetMessages(msgs)
		m.SetUserNames(a.userNames)
		if strings.Contains(stripANSI(m.View(20, 80)), "Seen by") {
			t.Error("a window opened later draws markers with read_receipts off")
		}
	}
	a.SetReadMarkers(true)
	if !strings.Contains(stripANSI(a.View().Content), "Seen by Bob") {
		t.Fatal("marker not drawn again")
	}
}

func TestSeenCommand(t *testing.T) {
	t.Run("Slack refuses it", func(t *testing.T) {
		a, _ := copyHuddleApp(t, withMessages(testMessageItems(3)...))
		keepToasts(t)
		a.SetReadReceiptService(&fakeReceipts{})
		focusMessages(t, a)
		runColonCommand(t, a, "seen")
		if got := statusbarText(a); !strings.Contains(got, "Slack has no read receipts") || a.mode != ModeNormal {
			t.Errorf("status bar = %q, mode %v", got, a.mode)
		}
	})
	t.Run("lists the readers of the selected message", func(t *testing.T) {
		a, _ := matrixApp(t)
		f := &fakeReceipts{readers: []core.Reader{
			{UserID: "U2", Name: "Bob", ReadTS: "1773565200.000000", ReadTime: "9:00 AM"},
			{UserID: "U3", Name: "Carol"},
		}}
		a.SetReadReceiptService(f)
		focusMessages(t, a)
		runColonCommand(t, a, "seen")
		sel, _ := a.messagepane.SelectedMessage()
		if want := []string{"C1  " + sel.TS}; !reflect.DeepEqual(f.asked, want) {
			t.Fatalf("asked %q, want %q", f.asked, want)
		}
		if a.mode != ModeReactionsView {
			t.Fatalf("mode = %v", a.mode)
		}
		if got := statusbarText(a); !strings.Contains(got, "SEEN") || strings.Contains(got, "REACTIONS") {
			t.Errorf("status bar = %q, want the SEEN mode label", got)
		}
		box := stripANSI(a.reactionsView.ViewOverlay(100, 30, ""))
		for _, w := range []string{"Read receipts", "Seen by  (2)", "Bob · ", "9:00 AM", "Carol"} {
			if !strings.Contains(box, w) {
				t.Errorf("%q not in the dialog:\n%s", w, box)
			}
		}
	})
	t.Run("a reply asks for its thread, the parent for the channel", func(t *testing.T) {
		a, _ := matrixApp(t)
		f := &fakeReceipts{}
		a.SetReadReceiptService(f)
		focusThreadPanel(t, a)
		runColonCommand(t, a, "seen")
		a.threadPanel.GoToTop()
		a.threadPanel.MoveUp() // the parent
		runColonCommand(t, a, "seen")
		sel, _ := a.threadPanel.SelectedMessage()
		if sel.TS != "10.0" {
			t.Fatalf("parent not selected: %q", sel.TS)
		}
		if want := []string{"C1 10.0 12.0", "C1  10.0"}; !reflect.DeepEqual(f.asked, want) {
			t.Errorf("asked %q, want %q", f.asked, want)
		}
		if got := statusbarText(a); !strings.Contains(got, "Nobody else has read this message yet") {
			t.Errorf("status bar = %q", got)
		}
	})
}

// TestSeenIsInMatrixHelpOnly: Slack has no read receipts, so its help
// overlay leaves :seen out.
func TestSeenIsInMatrixHelpOnly(t *testing.T) {
	listed := func(a *App) bool {
		for _, e := range help.FromKeyMap(a.helpKeyMap()) {
			if e.Key == ":seen" {
				return true
			}
		}
		return false
	}
	slack, _ := copyHuddleApp(t)
	matrix, _ := matrixApp(t)
	if listed(slack) || !listed(matrix) {
		t.Errorf(":seen in help: Slack %v, Matrix %v", listed(slack), listed(matrix))
	}
}
