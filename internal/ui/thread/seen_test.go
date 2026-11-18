package thread

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

// seenRows are the "Seen by" rows of a rendered view.
func seenRows(view string) []string {
	var out []string
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		if strings.Contains(l, "Seen by") {
			out = append(out, strings.TrimSpace(strings.Trim(l, "│▌ ")))
		}
	}
	return out
}

// TestThreadMoveSeen: a marker moving between replies re-renders only
// those replies, not the whole thread, and a removed reply hands its
// markers to the reply before it.
func TestThreadMoveSeen(t *testing.T) {
	m := newTestThread()
	m.SetUserNames(map[string]string{"U4": "Dave", "U5": "Erin"})
	_ = m.View(40, 60)
	m.MoveSeen("U4", "2.0")
	if m.cache == nil || len(m.staleReplies) != 1 {
		t.Fatalf("cache dropped %v, stale %v: want one reply re-rendered", m.cache == nil, m.staleReplies)
	}
	if got := seenRows(m.View(40, 60)); len(got) != 1 || !strings.HasSuffix(got[0], "Seen by Dave") {
		t.Fatalf("after the first move: %q", got)
	}
	m.MoveSeen("U4", "3.0")
	m.MoveSeen("U5", "3.0")
	view := ansi.Strip(m.View(40, 60))
	if got := seenRows(view); len(got) != 1 || !strings.HasSuffix(got[0], "Seen by Erin, Dave") ||
		strings.Index(view, "Seen by") < strings.Index(view, "second reply") {
		t.Fatalf("after moving on: %q\n%s", got, view)
	}
	if len(m.staleReplies) != 0 {
		t.Errorf("stale replies left after View: %v", m.staleReplies)
	}

	// bob (U2) wrote the first reply: his marker does not sit under it.
	m.MoveSeen("U2", "3.0")
	m.RemoveMessageByTS("3.0")
	if got := seenRows(m.View(40, 60)); len(got) != 1 || !strings.HasSuffix(got[0], "Seen by Erin, Dave") {
		t.Fatalf("after removing the reply: %q", got)
	}
}
