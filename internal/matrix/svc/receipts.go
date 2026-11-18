package svc

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// Other users' read receipts: the read markers under messages
// (core.MessageItem.SeenBy), kept live, and who has read a message
// (core.ReadReceiptService, :seen). Everything here reads the store;
// receipts reach it through rooms.Model (applyReceipts), which keeps only
// forward moves (store.AdvanceReceipt).
//
// A timeline is a room's main timeline (root "") or one thread (root its
// root event). Which receipts read a timeline, by thread_id (threaded
// receipts, MSC3771):
//
//	""        unthreaded: everything up to its event, main timeline and
//	          every thread
//	"main"    the main timeline only
//	a root    that thread only
//
// A user's position in a timeline is the latest of the receipts that
// read it, by slk TS; a receipt stored before its event was mapped (TS
// "") is mapped now, through the ident mapper, and left out while it
// still has none. Their marker sits under the newest shown message of the
// timeline at or before the position (a receipt on a reaction, an edit,
// a state event or a hidden message counts there), unless that message
// is their own; the account's own receipts never make a marker. In a
// thread the markers sit under replies: the root is a channel message,
// whose markers are the channel's.
//
// Live (sendMarkers): every receipt that moved in a room on screen sends
// a ReadMarkersMovedMsg per timeline it reads, with each mover's new
// marker message. An unthreaded receipt reads every thread, but only the
// thread last opened in its room (threadOpened) is told: the UI shows one
// thread at a time, and opening another reads its markers afresh.

// readPos is a user's position in one timeline.
type readPos struct {
	ts     string // the TS read up to
	readAt int64  // when, from the receipt that put it there (0 unknown)
}

// readsTimeline reports whether a receipt with thread_id thread reads the
// timeline of root ("" for the main timeline).
func readsTimeline(thread string, root id.EventID) bool {
	switch thread {
	case "":
		return true
	case string(event.ReadReceiptThreadMain):
		return root == ""
	}
	return root != "" && thread == string(root)
}

// positions is each other user's position in the timeline of root, from
// recs (receipts in room).
func (b *Backend) positions(ctx context.Context, room id.RoomID, root id.EventID, recs []store.Receipt) map[id.UserID]readPos {
	out := map[id.UserID]readPos{}
	for _, r := range recs {
		if r.UserID == b.self || !readsTimeline(r.ThreadID, root) {
			continue
		}
		ts := r.TS
		if ts == "" {
			mapped, ok, err := b.d.IDs.TS(ctx, room, r.EventID)
			if err != nil || !ok {
				continue
			}
			ts = mapped
		}
		if cur, ok := out[r.UserID]; !ok || ts > cur.ts {
			out[r.UserID] = readPos{ts: ts, readAt: r.ReadAt}
		}
	}
	return out
}

// withMarkers sets the read markers (SeenBy) of items, consecutive shown
// messages of the timeline of root, oldest first. upper is the TS of the
// first shown message after them ("" when they reach the newest): a
// position at or after it has its marker further on.
func (b *Backend) withMarkers(ctx context.Context, room id.RoomID, root id.EventID, items []core.MessageItem, upper string) {
	if len(items) == 0 {
		return
	}
	recs, err := b.d.Store.RoomReceipts(ctx, room)
	if err != nil {
		debuglog.Cache("matrix receipts: room=%s: %v", room, err)
		return
	}
	res := b.rooms.Resolver()
	type reader struct {
		alias string
		pos   readPos
	}
	at := map[int][]reader{}
	for u, p := range b.positions(ctx, room, root, recs) {
		if upper != "" && p.ts >= upper {
			continue
		}
		i := sort.Search(len(items), func(i int) bool { return items[i].TS > p.ts }) - 1
		if i < 0 {
			continue
		}
		alias := res.UserAlias(u)
		if items[i].UserID == alias {
			continue
		}
		at[i] = append(at[i], reader{alias, p})
	}
	for i, rs := range at {
		slices.SortFunc(rs, func(x, y reader) int {
			return cmp.Or(cmp.Compare(y.pos.ts, x.pos.ts), cmp.Compare(y.pos.readAt, x.pos.readAt), cmp.Compare(x.alias, y.alias))
		})
		seen := make([]string, len(rs))
		for j, r := range rs {
			seen[j] = r.alias
		}
		items[i].SeenBy = seen
	}
}

// nextShownTS is the TS of room's first shown channel message after ts,
// "" when there is none.
func (b *Backend) nextShownTS(ctx context.Context, room id.RoomID, ts string) (string, error) {
	const batch = 8
	for {
		rows, err := b.rowsFrom(ctx, room, ts+"\x00", batch)
		if err != nil || len(rows) == 0 {
			return "", err
		}
		items, err := b.items(ctx, room, rows)
		if err != nil {
			return "", err
		}
		if len(items) > 0 {
			return items[0].TS, nil
		}
		if len(rows) < batch {
			return "", nil
		}
		ts = rows[len(rows)-1].TS
	}
}

// markerAt is the message user's marker sits under in the timeline of
// root: the newest shown message at or before their position, "" when
// there is none or it is their own.
func (b *Backend) markerAt(ctx context.Context, room id.RoomID, root id.EventID, user id.UserID) (string, error) {
	recs, err := b.d.Store.UserRoomReceipts(ctx, room, user)
	if err != nil {
		return "", err
	}
	p, ok := b.positions(ctx, room, root, recs)[user]
	if !ok {
		return "", nil
	}
	var it core.MessageItem
	if root == "" {
		before := p.ts + "\x00" // storedRows reads before, exclusive
		for {
			rows, more, err := b.storedRows(ctx, room, before, 8)
			if err != nil || len(rows) == 0 {
				return "", err
			}
			items, err := b.items(ctx, room, rows)
			if err != nil {
				return "", err
			}
			if n := len(items); n > 0 {
				it = items[n-1]
				break
			}
			if !more {
				return "", nil
			}
			before = rows[0].TS
		}
	} else {
		rows, err := b.threadRows(ctx, room, root)
		if err != nil {
			return "", err
		}
		rows = slices.DeleteFunc(rows, func(e store.Event) bool { return e.ID == root || e.TS > p.ts })
		items, err := b.items(ctx, room, rows)
		if err != nil || len(items) == 0 {
			return "", err
		}
		it = items[len(items)-1]
	}
	if it.UserID == b.rooms.Resolver().UserAlias(user) {
		return "", nil
	}
	return it.TS, nil
}

// threadOpened records root as the thread of room the UI last opened, for
// sendMarkers.
func (b *Backend) threadOpened(room id.RoomID, root id.EventID) {
	b.mu.Lock()
	b.openThread[room] = root
	b.mu.Unlock()
}

// sendMarkers tells the UI where the markers of the users whose receipts
// moved (recs, from one batch) are now, in the rooms on screen.
func (r *liveRun) sendMarkers(ctx context.Context, recs []rooms.UserReceipt) {
	b := r.b
	type timeline struct {
		room id.RoomID
		root id.EventID
	}
	var order []timeline
	moved := map[timeline]map[id.UserID]bool{}
	add := func(tl timeline, u id.UserID) {
		if moved[tl] == nil {
			moved[tl] = map[id.UserID]bool{}
			order = append(order, tl)
		}
		moved[tl][u] = true
	}
	for _, rec := range recs {
		if rec.User == b.self {
			continue
		}
		room, ok := b.rooms.RoomByID(rec.Room)
		if !ok || !b.inActive(room.Alias) {
			continue
		}
		switch rec.Thread {
		case "":
			add(timeline{rec.Room, ""}, rec.User)
			b.mu.Lock()
			root := b.openThread[rec.Room]
			b.mu.Unlock()
			if root != "" {
				add(timeline{rec.Room, root}, rec.User)
			}
		case string(event.ReadReceiptThreadMain):
			add(timeline{rec.Room, ""}, rec.User)
		default:
			add(timeline{rec.Room, id.EventID(rec.Thread)}, rec.User)
		}
	}
	res := b.rooms.Resolver()
	for _, tl := range order {
		room, _ := b.rooms.RoomByID(tl.room)
		msg := ui.ReadMarkersMovedMsg{ChannelID: room.Alias}
		if tl.root != "" {
			ts, ok, err := b.d.IDs.TS(ctx, tl.room, tl.root)
			if err != nil || !ok {
				continue
			}
			msg.ThreadTS = ts
		}
		for _, u := range slices.Sorted(maps.Keys(moved[tl])) {
			ts, err := b.markerAt(ctx, tl.room, tl.root, u)
			if err != nil {
				debuglog.General("matrix live: read marker of %s in %s: %v", u, tl.room, err)
				continue
			}
			msg.Moves = append(msg.Moves, ui.SeenMove{UserID: res.UserAlias(u), TS: ts})
		}
		if len(msg.Moves) > 0 {
			r.send(msg)
		}
	}
}

// receiptService is the backend's core.ReadReceiptService.
type receiptService struct{ b *Backend }

var _ core.ReadReceiptService = receiptService{}

// SeenBy lists the users whose position in the message's timeline is at
// or after it; see core.ReadReceiptService. The thread parent is a
// channel message: asked with its own TS as threadTS, the channel's
// positions answer.
func (s receiptService) SeenBy(channelID, threadTS, ts string) ([]core.Reader, error) {
	b := s.b
	ctx, cancel, room, err := b.roomCtx(channelID)
	if err != nil {
		return nil, err
	}
	defer cancel()
	ev, ok, err := b.d.IDs.Event(ctx, room, ts)
	if err == nil && !ok {
		err = errors.New("svc: no message with that TS")
	}
	if err != nil {
		return nil, err
	}
	msg, _, err := b.d.Store.GetEvent(ctx, room, ev)
	if err != nil {
		return nil, err
	}
	var root id.EventID
	if threadTS != "" && threadTS != ts {
		if root, ok, err = b.d.IDs.Event(ctx, room, threadTS); err != nil || !ok {
			return nil, cmp.Or(err, errors.New("svc: no thread with that TS"))
		}
	}
	recs, err := b.d.Store.RoomReceipts(ctx, room)
	if err != nil {
		return nil, err
	}
	type reader struct {
		r   core.Reader
		pos readPos
	}
	var rs []reader
	res := b.rooms.Resolver()
	for u, p := range b.positions(ctx, room, root, recs) {
		if p.ts < ts || u == msg.Sender {
			continue
		}
		r := core.Reader{UserID: res.UserAlias(u), Name: b.rooms.DisplayName(room, u)}
		if p.readAt > 0 {
			at := time.UnixMilli(p.readAt)
			r.ReadTS = fmt.Sprintf("%010d.%06d", at.Unix(), at.Nanosecond()/1000)
			r.ReadTime = at.Format(b.tsFormat)
		}
		rs = append(rs, reader{r, p})
	}
	// Newest read first: by when they read, else by how far.
	slices.SortFunc(rs, func(x, y reader) int {
		return cmp.Or(cmp.Compare(y.pos.readAt, x.pos.readAt), cmp.Compare(y.pos.ts, x.pos.ts),
			cmp.Compare(x.r.Name, y.r.Name), cmp.Compare(x.r.UserID, y.r.UserID))
	})
	out := make([]core.Reader, len(rs))
	for i, r := range rs {
		out[i] = r.r
	}
	return out, nil
}
