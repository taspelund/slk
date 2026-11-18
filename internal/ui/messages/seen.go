package messages

import (
	"slices"
	"strconv"
	"strings"

	"charm.land/lipgloss/v2"
)

// The read markers ("Seen by Alice, Bob"): a row under the newest message
// each other user has read, from MessageItem.SeenBy. Both panes draw it
// through RenderMessage, unless RenderContext.HideSeen ([general]
// read_receipts off); the panes move it live with MoveSeen.

// SeenByText is the read-marker row for readers (user IDs, in order):
// "Seen by " and their names (names[id], else the ID), as many as fit
// width cells, the rest counted ("Seen by Alice, Bob +12"); "" when
// there are none. A name's whitespace runs (newlines included) become one
// space, so a name cannot break the row.
func SeenByText(readers []string, names map[string]string, width int) string {
	if len(readers) == 0 {
		return ""
	}
	const prefix = "Seen by "
	more := func(n int) string {
		if n == 0 {
			return ""
		}
		return " +" + strconv.Itoa(n)
	}
	shown := make([]string, len(readers))
	used := lipgloss.Width(prefix)
	fit := 0
	for i, id := range readers {
		name := names[id]
		if name == "" {
			name = id
		}
		shown[i] = strings.Join(strings.Fields(name), " ")
		if i > 0 {
			used += 2 // ", "
		}
		used += lipgloss.Width(shown[i])
		if used+lipgloss.Width(more(len(readers)-i-1)) > width {
			break
		}
		fit = i + 1
	}
	if fit == 0 {
		return prefix + strings.TrimPrefix(more(len(readers)), " ")
	}
	return prefix + strings.Join(shown[:fit], ", ") + more(len(readers)-fit)
}

// MoveSeenBy returns seen (a message's SeenBy) with user removed, and with
// user first when here says the user's marker belongs on this message.
// changed is false when that is already so; seen itself is never
// written (copies of a message share the slice).
func MoveSeenBy(seen []string, user string, here bool) (out []string, changed bool) {
	i := slices.Index(seen, user)
	switch {
	case here && i >= 0, !here && i < 0:
		return seen, false
	case here:
		return append([]string{user}, seen...), true
	}
	return slices.Delete(slices.Clone(seen), i, i+1), true
}
