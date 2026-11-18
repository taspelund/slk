package messages

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestSeenByText(t *testing.T) {
	names := map[string]string{"U1": "Alice", "U2": "Bob", "U3": "Carol", "U4": "Dana\nEvil"}
	for _, c := range []struct {
		name    string
		readers []string
		width   int
		want    string
	}{
		{"none", nil, 80, ""},
		{"all fit", []string{"U1", "U2"}, 80, "Seen by Alice, Bob"},
		{"exactly fits", []string{"U1", "U2"}, len("Seen by Alice, Bob"), "Seen by Alice, Bob"},
		{"the rest counted", []string{"U1", "U2", "U3"}, len("Seen by Alice, Bob +1"), "Seen by Alice, Bob +1"},
		{"counted with room for the count", []string{"U1", "U2", "U3"}, len("Seen by Alice, Bob, Carol") - 1, "Seen by Alice, Bob +1"},
		{"no name fits", []string{"U1", "U2", "U3"}, 10, "Seen by +3"},
		{"unknown name is the ID", []string{"U9"}, 80, "Seen by U9"},
		{"a name cannot break the row", []string{"U4"}, 80, "Seen by Dana Evil"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := SeenByText(c.readers, names, c.width); got != c.want {
				t.Errorf("SeenByText = %q, want %q", got, c.want)
			}
		})
	}
}

func TestMoveSeenBy(t *testing.T) {
	shared := []string{"U1", "U2"}
	got, changed := MoveSeenBy(shared, "U1", false)
	if !changed || strings.Join(got, ",") != "U2" || strings.Join(shared, ",") != "U1,U2" {
		t.Fatalf("remove: %v %v (shared %v)", got, changed, shared)
	}
	got, changed = MoveSeenBy(shared, "U3", true)
	if !changed || strings.Join(got, ",") != "U3,U1,U2" || strings.Join(shared, ",") != "U1,U2" {
		t.Fatalf("add: %v %v (shared %v)", got, changed, shared)
	}
	if _, changed := MoveSeenBy(shared, "U2", true); changed {
		t.Fatal("already there: changed")
	}
	if _, changed := MoveSeenBy(shared, "U3", false); changed {
		t.Fatal("not there: changed")
	}
}

// seenRows is the plain rows of the pane that hold a read marker.
func seenRows(view string) []string {
	var out []string
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(l, "Seen by") {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return out
}

// TestMoveSeenRerendersOnlyTheMessagesItTouches: a marker moving takes it
// off the old message and puts it under the new one, through the
// per-entry rebuild, not a whole-pane one.
func TestMoveSeenRerendersOnlyTheMessagesItTouches(t *testing.T) {
	msgs := []MessageItem{
		{TS: "1.0", UserID: "U1", UserName: "alice", Text: "one", Timestamp: "1:00 PM", SeenBy: []string{"U2"}},
		{TS: "2.0", UserID: "U1", UserName: "alice", Text: "two", Timestamp: "1:01 PM"},
		{TS: "3.0", UserID: "U1", UserName: "alice", Text: "three", Timestamp: "1:02 PM"},
	}
	m := New(msgs, "general")
	m.SetUserNames(map[string]string{"U2": "Bob", "U3": "Carol"})
	view := m.View(30, 60)
	if got := seenRows(view); len(got) != 1 || !strings.HasSuffix(got[0], "Seen by Bob") {
		t.Fatalf("before: %q", got)
	}
	m.MoveSeen("U2", "3.0")
	if m.cache == nil || len(m.staleEntries) != 2 {
		t.Fatalf("cache dropped %v, stale %v: want the two messages re-rendered alone", m.cache == nil, m.staleEntries)
	}
	m.MoveSeen("U3", "3.0")
	view = m.View(30, 60)
	plain := ansi.Strip(view)
	if got := seenRows(view); len(got) != 1 || !strings.HasSuffix(got[0], "Seen by Carol, Bob") ||
		strings.Index(plain, "Seen by") < strings.Index(plain, "three") {
		t.Fatalf("after: %q\n%s", got, plain)
	}
	// The message the user left keeps no marker; one not loaded only
	// takes it off.
	m.MoveSeen("U2", "9.0")
	if got := seenRows(m.View(30, 60)); len(got) != 1 || !strings.HasSuffix(got[0], "Seen by Carol") {
		t.Fatalf("moved to a message not loaded: %q", got)
	}

	// Hidden by the option, still moving.
	m.SetHideSeen(true)
	m.MoveSeen("U2", "2.0")
	if got := seenRows(m.View(30, 60)); len(got) != 0 {
		t.Fatalf("hidden: %q", got)
	}
	m.SetHideSeen(false)
	if got := seenRows(m.View(30, 60)); len(got) != 2 {
		t.Fatalf("shown again: %q", got)
	}
}
