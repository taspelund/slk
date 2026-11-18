package svc

import (
	"context"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/rooms"
)

// linkTimeout bounds the store reads of one ResolveLink, which runs on
// the UI goroutine.
const linkTimeout = 2 * time.Second

// linkResolver is the backend's core.LinkResolver: matrix.to links (and
// matrix: URIs) to rooms the account has joined, resolved from the room
// model and the store, never the network.
//
//	https://matrix.to/#/!room:server, /#/#alias:server   the room
//	https://matrix.to/#/!room:server/$event?via=...      the message,
//	https://matrix.to/#/#alias:server/$event              or the reply in
//	                                                      its thread
//
// An alias is the room's canonical alias. A room that is unknown, not
// joined or a space is not resolved (the UI opens the link in the
// browser). An event the store does not hold, or a thread reply whose
// root has no TS, resolves to the room alone; an edit or a reaction to
// the message it relates to.
type linkResolver struct{ b *Backend }

var _ core.LinkResolver = linkResolver{}

// ResolveLink implements core.LinkResolver.
func (l linkResolver) ResolveLink(raw string) (core.LinkTarget, bool) {
	uri, err := id.ParseMatrixURIOrMatrixToURL(raw)
	if err != nil || uri == nil {
		return core.LinkTarget{}, false
	}
	b := l.b
	var room rooms.Room
	var ok bool
	switch uri.Sigil1 {
	case '!':
		room, ok = b.rooms.RoomByID(uri.RoomID())
	case '#':
		room, ok = b.rooms.RoomByCanonicalAlias(uri.RoomAlias())
	}
	if !ok || room.Section == "" {
		// Section is "" for a room that is not a sidebar row: an
		// invite, a room left, a space.
		return core.LinkTarget{}, false
	}
	t := core.LinkTarget{ChannelID: ids.ChannelID(room.Alias)}
	ev := uri.EventID()
	if ev == "" {
		return t, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), linkTimeout)
	defer cancel()
	e, found, err := b.d.Store.GetEvent(ctx, room.ID, ev)
	if err == nil && found && e.RelatesTo != "" &&
		(e.RelType == string(event.RelReplace) || e.RelType == string(event.RelAnnotation)) {
		e, found, err = b.d.Store.GetEvent(ctx, room.ID, e.RelatesTo)
	}
	if err != nil || !found || e.TS == "" {
		return t, true
	}
	if e.ThreadRoot != "" && e.ThreadRoot != e.ID {
		root, ok, err := b.d.IDs.TS(ctx, room.ID, e.ThreadRoot)
		if err != nil || !ok {
			return t, true
		}
		t.ThreadTS = ids.ThreadTS(root)
	}
	t.MessageTS = ids.MessageTS(e.TS)
	return t, true
}
