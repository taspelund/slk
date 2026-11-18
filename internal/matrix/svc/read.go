package svc

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/avatar"
	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/text"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// The read side: channel history, threads, unread state, search and
// avatars.
//
// History comes from the store; the server fills it in, within a budget
// of serverPages requests per Fetch or FetchOlder, before the store is
// read again for the page:
//
//   - Gaps (store.Gaps, holes a limited /sync leaves). A page read crosses
//     a gap when the gap's after_ts is inside the page: above its oldest
//     row (or anywhere below the top when the store has nothing older
//     than the page) and at or below its top (the TS FetchOlder pages
//     before; no limit for Fetch's newest page). The newest crossed gap
//     is paged with /messages from its prev_batch and the page handed to
//     rooms.Model.FillGap, which places the events in the hole and
//     removes or moves the gap; then the page is read again, until no
//     gap crosses it or the budget is spent. So Fetch fills the holes
//     inside the newest page, and FetchOlder those it meets going back.
//     A hole above FetchOlder's anchor (inside the buffer on screen) is
//     not filled by it: the events would land amid messages already
//     shown; the next Fetch fills it if it is in the newest page.
//   - The room's start. When the page is still short and the store has
//     nothing older, paginate pages back from the oldest stored event
//     (backTokenFor) and hands the events to rooms.Model.Backfill.
//
// Threads (/relations) and /context windows go through Backfill too,
// which places each event among the stored ones by its origin time.

const (
	// pageSize is how many message rows a history page holds.
	pageSize = 50
	// serverPage is the /messages limit; serverPages bounds the requests
	// one fetch makes.
	serverPage  = 100
	serverPages = 4
	// fetchTimeout bounds one port call, network included.
	fetchTimeout = 30 * time.Second
	// threadPages bounds the /relations pages of one thread fetch.
	threadPages = 10
	// threadSweepEvery is how often ListFetch asks the server for the
	// rooms' threads; threadsListed is how many summaries it returns.
	threadSweepEvery = 5 * time.Minute
	threadsListed    = 50
)

// errUnknownRoom is a channel alias the room model does not know.
var errUnknownRoom = errors.New("svc: unknown room")

// fillRead fills the read side of the ports (WP-Read).
func (b *Backend) fillRead(p *ports) {
	p.channels.Fetch = func(ch ids.ChannelID, _ string) core.Msg {
		// p.channels.MarkRead is read when Fetch runs, after Install
		// has run fillWrite; nothing writes it after that.
		return b.fetch(string(ch), p.channels.MarkRead)
	}
	p.channels.FetchOlder = func(ch ids.ChannelID, oldest ids.MessageTS) core.Msg {
		return b.fetchOlder(string(ch), string(oldest))
	}
	p.channels.FetchAround = func(ch ids.ChannelID, ts ids.MessageTS) core.Msg {
		return b.fetchAround(string(ch), string(ts))
	}
	p.channels.ReadCache = func(ch ids.ChannelID) []core.MessageItem { return b.readCache(string(ch)) }
	p.channels.SyncedAt = func(ch ids.ChannelID) int64 { return b.syncedAt(string(ch)) }
	p.channels.Lookup = func(ch ids.ChannelID) (string, string, bool) {
		r, ok := b.rooms.Room(string(ch))
		return r.Name, string(r.Kind), ok
	}
	// RecordVisit stays unset: the finder already orders by visits in
	// process (channelFinder.UpdateLastVisited), and the store has no
	// place to keep them across restarts.

	p.threads.Fetch = func(ch ids.ChannelID, ts ids.ThreadTS) core.Msg { return b.fetchThread(string(ch), string(ts)) }
	p.threads.CacheRead = func(ch ids.ChannelID, ts ids.ThreadTS) []core.MessageItem {
		return b.threadCache(string(ch), string(ts))
	}
	p.threads.ThreadLastRead = func(ch ids.ChannelID, ts ids.ThreadTS) string {
		return b.threadLastRead(string(ch), string(ts))
	}
	p.threads.ListFetch = func(team ids.TeamID) core.Msg { return b.listThreads(string(team)) }

	p.search.SearchChannel = func(ch ids.ChannelID, query string) core.Msg {
		return b.searchChannel(string(ch), query)
	}
	p.unread = core.NewUnreadService(b.readStates, b.unreadWorkspaces)
	p.workspace = core.NewWorkspaceService(b.switchTo)
	p.avatars = core.NewAvatarService(b.avatar, nil)
}

// roomCtx resolves a channel alias and starts a port call's context.
func (b *Backend) roomCtx(alias string) (context.Context, context.CancelFunc, id.RoomID, error) {
	r, ok := b.rooms.Room(alias)
	if !ok {
		return nil, nil, "", errUnknownRoom
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	return ctx, cancel, r.ID, nil
}

// fetch is ChannelService.Fetch: the newest page, paging back on the
// server when the store holds less. It marks the room read at the newest
// message through markRead (WP-Write's MarkRead) when that is set; the
// receipt's /sync echo then updates the read state.
func (b *Backend) fetch(alias string, markRead func(ids.ChannelID, ids.MessageTS) core.Msg) core.Msg {
	ctx, cancel, room, err := b.roomCtx(alias)
	if err != nil {
		return ui.MessagesLoadedMsg{ChannelID: alias}
	}
	defer cancel()
	b.opened(alias)
	msgs, err := b.page(ctx, room, "")
	if err != nil {
		debuglog.Cache("matrix fetch: room=%s: %v", room, err)
		return ui.MessagesLoadedMsg{ChannelID: alias} // nil: keep what is shown
	}
	b.withMarkers(ctx, room, "", msgs, "")
	b.mu.Lock()
	b.synced[alias] = time.Now()
	b.mu.Unlock()
	out := ui.MessagesLoadedMsg{
		ChannelID:  alias,
		Messages:   msgs,
		LastReadTS: b.readStates()[alias].LastReadTS,
	}
	if n := len(msgs); n > 0 && markRead != nil {
		out.MarkedTS = msgs[n-1].TS
		go markRead(ids.ChannelID(alias), ids.MessageTS(out.MarkedTS))
	}
	return out
}

// fetchOlder is ChannelService.FetchOlder.
func (b *Backend) fetchOlder(alias, oldest string) core.Msg {
	out := ui.OlderMessagesLoadedMsg{ChannelID: alias, AnchorTS: oldest}
	ctx, cancel, room, err := b.roomCtx(alias)
	if err != nil {
		return out
	}
	defer cancel()
	if out.Messages, err = b.page(ctx, room, oldest); err != nil {
		debuglog.Cache("matrix fetch older: room=%s before=%s: %v", room, oldest, err)
	}
	// The page ends where the messages shown begin (oldest).
	b.withMarkers(ctx, room, "", out.Messages, oldest)
	return out
}

// page returns the page of message items before before ("" for the
// newest), paging back on the server when the store has too few.
func (b *Backend) page(ctx context.Context, room id.RoomID, before string) ([]core.MessageItem, error) {
	rows, more, err := b.storedRows(ctx, room, before, pageSize)
	if err != nil {
		return nil, err
	}
	if b.client != nil {
		budget := serverPages
		for budget > 0 {
			g, ok, err := b.crossedGap(ctx, room, rows, more, before)
			if err != nil {
				return nil, err
			}
			if !ok {
				break
			}
			budget--
			if err := b.fillGap(ctx, room, g); err != nil {
				debuglog.Cache("matrix fill gap: room=%s gap=%d: %v", room, g.ID, err)
				break
			}
			if rows, more, err = b.storedRows(ctx, room, before, pageSize); err != nil {
				return nil, err
			}
		}
		if len(rows) < pageSize && !more && budget > 0 {
			if err := b.paginate(ctx, room, pageSize-len(rows), budget); err != nil {
				debuglog.Cache("matrix paginate: room=%s: %v", room, err)
			}
			if rows, _, err = b.storedRows(ctx, room, before, pageSize); err != nil {
				return nil, err
			}
		}
	}
	items, err := b.items(ctx, room, rows)
	if items == nil && err == nil {
		items = []core.MessageItem{}
	}
	return items, err
}

// crossedGap returns the newest gap of room inside the page rows (read
// before before, with more from storedRows); see the top of this file.
func (b *Backend) crossedGap(ctx context.Context, room id.RoomID, rows []store.Event, more bool, before string) (store.Gap, bool, error) {
	gaps, err := b.d.Store.Gaps(ctx, room)
	if err != nil {
		return store.Gap{}, false, err
	}
	lower := ""
	if more && len(rows) > 0 {
		lower = rows[0].TS
	}
	for i := len(gaps) - 1; i >= 0; i-- { // newest after_ts first
		g := gaps[i]
		if g.AfterTS > lower && (before == "" || g.AfterTS <= before) {
			return g, true, nil
		}
	}
	return store.Gap{}, false, nil
}

// fillGap pages back once from gap g's prev_batch and hands the page to
// the room model, which fills the hole and removes or moves the gap. A
// page that reaches the room's start also ends paging back.
func (b *Backend) fillGap(ctx context.Context, room id.RoomID, g store.Gap) error {
	l := b.pageLock(room)
	l.Lock()
	defer l.Unlock()
	if _, ok, err := b.d.Store.Gap(ctx, g.ID); err != nil || !ok {
		return err // another fetch filled it meanwhile
	}
	resp, err := b.client.Messages(ctx, room, g.PrevBatch, "", mautrix.DirectionBackward, lazyMembers, serverPage)
	if err != nil {
		return err
	}
	next := resp.End
	if len(resp.Chunk) == 0 {
		next = ""
	}
	b.prepareAll(ctx, room, resp.Chunk, resp.State)
	if _, err := b.rooms.FillGap(ctx, room, g.ID, resp.Chunk, resp.State, next, false); err != nil {
		return err
	}
	if next == "" {
		_, err = b.remember(ctx, room, "")
	}
	return err
}

// storedRows returns up to n of room's channel rows before before ("" for
// the newest), oldest first, and whether the store may hold more before
// them.
func (b *Backend) storedRows(ctx context.Context, room id.RoomID, before string, n int) ([]store.Event, bool, error) {
	var out []store.Event // newest first
	for {
		batch, err := b.d.Store.EventsBefore(ctx, room, before, 2*n)
		if err != nil {
			return nil, false, err
		}
		for i := len(batch) - 1; i >= 0; i-- {
			if isMainRow(batch[i]) {
				out = append(out, batch[i])
				if len(out) == n {
					slices.Reverse(out)
					return out, true, nil
				}
			}
		}
		if len(batch) < 2*n {
			slices.Reverse(out)
			return out, false, nil
		}
		before = batch[0].TS
	}
}

// rowsFrom returns up to n of room's channel rows at or after from,
// oldest first.
func (b *Backend) rowsFrom(ctx context.Context, room id.RoomID, from string, n int) ([]store.Event, error) {
	var out []store.Event
	for {
		batch, err := b.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
			WHERE room_id = $1 AND ts >= $2 ORDER BY ts LIMIT $3`, room, from, 2*n)
		if err != nil {
			return nil, err
		}
		for _, e := range batch {
			if isMainRow(e) {
				out = append(out, e)
				if len(out) == n {
					return out, nil
				}
			}
		}
		if len(batch) < 2*n {
			return out, nil
		}
		// Past the last one: TS values have one length, so appending
		// to one sorts it before every greater TS.
		from = batch[len(batch)-1].TS + "\x00"
	}
}

// backToken is where paging back in a room continues from. It is kept in
// memory: the store has no column for it, and it is cheap to find again
// (backTokenFor). Token and Done describe the position before the stored
// event with TS Oldest ("" when the room had no stored events).
type backToken struct {
	Oldest string
	Token  string
	Done   bool // the room's history start was reached
}

// oldestEvent returns room's oldest stored event.
func (b *Backend) oldestEvent(ctx context.Context, room id.RoomID) (store.Event, bool, error) {
	evs, err := b.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
		WHERE room_id = $1 ORDER BY ts LIMIT 1`, room)
	if err != nil || len(evs) == 0 {
		return store.Event{}, false, err
	}
	return evs[0], true, nil
}

// backTokenFor returns where paging back in room continues: the token
// remembered for the current oldest stored event, else, from /context of
// that event, the token before it. A room with nothing stored pages from
// the live end (""). A gap at the oldest event (a limited first /sync)
// is not used here: page fills it through FillGap first.
func (b *Backend) backTokenFor(ctx context.Context, room id.RoomID) (backToken, error) {
	oldest, ok, err := b.oldestEvent(ctx, room)
	if err != nil {
		return backToken{}, err
	}
	b.mu.Lock()
	t, known := b.back[room]
	b.mu.Unlock()
	if !ok {
		return t, nil // the token after an empty page, or "": the live end
	}
	if oldest.Type == event.StateCreate.Type {
		return backToken{Oldest: oldest.TS, Done: true}, nil
	}
	if known && t.Oldest == oldest.TS {
		return t, nil
	}
	resp, err := b.client.Context(ctx, room, oldest.ID, lazyMembers, 2)
	if err != nil {
		return backToken{}, err
	}
	if err := b.backfill(ctx, room, resp.EventsBefore, resp.State); err != nil {
		return backToken{}, err
	}
	return b.remember(ctx, room, resp.Start)
}

// remember records token as the position before room's oldest stored
// event, "" meaning the history start was reached.
func (b *Backend) remember(ctx context.Context, room id.RoomID, token string) (backToken, error) {
	oldest, _, err := b.oldestEvent(ctx, room)
	if err != nil {
		return backToken{}, err
	}
	t := backToken{Oldest: oldest.TS, Token: token, Done: token == ""}
	b.mu.Lock()
	b.back[room] = t
	b.mu.Unlock()
	return t, nil
}

// lazyMembers asks for the senders' member events with history.
var lazyMembers = &mautrix.FilterPart{LazyLoadMembers: true}

// paginate pages back in room until about want more channel rows are
// stored, the history start is reached, or budget requests are made.
func (b *Backend) paginate(ctx context.Context, room id.RoomID, want, budget int) error {
	l := b.pageLock(room)
	l.Lock()
	defer l.Unlock()
	for range budget {
		t, err := b.backTokenFor(ctx, room)
		if err != nil || t.Done {
			return err
		}
		resp, err := b.client.Messages(ctx, room, t.Token, "", mautrix.DirectionBackward, lazyMembers, serverPage)
		if err != nil {
			return err
		}
		if err := b.backfill(ctx, room, resp.Chunk, resp.State); err != nil {
			return err
		}
		next := resp.End
		if len(resp.Chunk) == 0 {
			next = ""
		}
		if _, err := b.remember(ctx, room, next); err != nil {
			return err
		}
		for _, e := range resp.Chunk {
			if e.Type == event.EventMessage || e.Type == event.EventEncrypted || e.Type == event.EventSticker {
				want--
			}
		}
		if want <= 0 || next == "" {
			return nil
		}
	}
	return nil
}

// backfill prepares fetched events (in the orders rooms.Model.Backfill
// documents) and hands them to the room model.
func (b *Backend) backfill(ctx context.Context, room id.RoomID, evs, state []*event.Event) error {
	if len(evs) == 0 {
		return nil
	}
	b.prepareAll(ctx, room, evs, state)
	_, err := b.rooms.Backfill(ctx, room, evs, state)
	return err
}

// prepareAll prepares a fetched page's events and state in place.
func (b *Backend) prepareAll(ctx context.Context, room id.RoomID, evs, state []*event.Event) {
	for i, e := range evs {
		evs[i] = b.prepare(ctx, room, e, true)
	}
	for i, e := range state {
		state[i] = b.prepare(ctx, room, e, false)
	}
}

// prepare gives a fetched event what /sync's events have: its room, its
// type class, parsed content, and decryption where the keys are on hand
// (one that does not decrypt stays m.room.encrypted, as from /sync).
func (b *Backend) prepare(ctx context.Context, room id.RoomID, e *event.Event, timeline bool) *event.Event {
	if e.RoomID == "" {
		e.RoomID = room
	}
	switch {
	case e.StateKey != nil:
		e.Type.Class = event.StateEventType
	case timeline:
		e.Type.Class = event.MessageEventType
	}
	_ = e.Content.ParseRaw(e.Type)
	if e.Type != event.EventEncrypted || b.client == nil || b.client.Crypto == nil {
		return e
	}
	dec, err := b.client.Crypto.Decrypt(ctx, e)
	if err != nil {
		return e
	}
	dec.Mautrix.EventSource |= event.SourceDecrypted
	return dec
}

// fetchAround is ChannelService.FetchAround: the page around ts. An event
// with a TS that is no longer stored is fetched with /context first.
func (b *Backend) fetchAround(alias, ts string) core.Msg {
	out := ui.MessagesAroundLoadedMsg{ChannelID: alias, TargetTS: ts}
	ctx, cancel, room, err := b.roomCtx(alias)
	if err != nil {
		out.Err = err
		return out
	}
	defer cancel()
	out.Messages, out.Err = b.around(ctx, room, ts)
	return out
}

func (b *Backend) around(ctx context.Context, room id.RoomID, ts string) ([]core.MessageItem, error) {
	ev, ok, err := b.d.IDs.Event(ctx, room, ts)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("svc: no message with that TS")
	}
	if _, stored, err := b.d.Store.GetEvent(ctx, room, ev); err != nil {
		return nil, err
	} else if !stored && b.client != nil {
		resp, err := b.client.Context(ctx, room, ev, lazyMembers, pageSize)
		if err != nil {
			return nil, err
		}
		evs := slices.Clone(resp.EventsAfter)
		slices.Reverse(evs) // events_after is oldest first; Backfill takes newest first
		evs = append(evs, resp.Event)
		evs = append(evs, resp.EventsBefore...)
		if err := b.backfill(ctx, room, evs, resp.State); err != nil {
			return nil, err
		}
	}
	before, _, err := b.storedRows(ctx, room, ts, pageSize/2)
	if err != nil {
		return nil, err
	}
	after, err := b.rowsFrom(ctx, room, ts, pageSize/2)
	if err != nil {
		return nil, err
	}
	items, err := b.items(ctx, room, append(before, after...))
	if err != nil || len(items) == 0 {
		return items, err
	}
	upper, err := b.nextShownTS(ctx, room, items[len(items)-1].TS)
	if err != nil {
		return nil, err
	}
	b.withMarkers(ctx, room, "", items, upper)
	return items, nil
}

// readCache is ChannelService.ReadCache: the newest stored page, without
// the server.
func (b *Backend) readCache(alias string) []core.MessageItem {
	ctx, cancel, room, err := b.roomCtx(alias)
	if err != nil {
		return nil
	}
	defer cancel()
	rows, _, err := b.storedRows(ctx, room, "", pageSize)
	if err != nil || len(rows) == 0 {
		return nil
	}
	items, err := b.items(ctx, room, rows)
	if err != nil || len(items) == 0 {
		return nil
	}
	b.withMarkers(ctx, room, "", items, "")
	return items
}

// syncedAt is ChannelService.SyncedAt: when Fetch last loaded the room.
// /sync keeps the store current between fetches, so a recent Fetch makes
// the store as good as a new one.
func (b *Backend) syncedAt(alias string) int64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	if t, ok := b.synced[alias]; ok {
		return t.Unix()
	}
	return 0
}

// threadRoot resolves a thread's channel alias and root TS.
func (b *Backend) threadRoot(ctx context.Context, alias, ts string) (id.RoomID, id.EventID, error) {
	r, ok := b.rooms.Room(alias)
	if !ok {
		return "", "", errUnknownRoom
	}
	root, ok, err := b.d.IDs.Event(ctx, r.ID, ts)
	if err == nil && !ok {
		err = errors.New("svc: no message with that TS")
	}
	return r.ID, root, err
}

// threadRows returns a thread's stored root (if stored) and replies, in
// TS order.
func (b *Backend) threadRows(ctx context.Context, room id.RoomID, root id.EventID) ([]store.Event, error) {
	evs, err := b.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
		WHERE room_id = $1 AND (event_id = $2 OR thread_root = $2 OR (rel_type = 'm.thread' AND relates_to_event = $2))
		ORDER BY ts`, room, root)
	if err != nil {
		return nil, err
	}
	return slices.DeleteFunc(evs, func(e store.Event) bool { return !isRow(e) }), nil
}

// threadItems returns the thread's root item (ok false when it is not
// stored) and its reply items.
func (b *Backend) threadItems(ctx context.Context, room id.RoomID, root id.EventID, rootTS string) (core.MessageItem, bool, []core.MessageItem, error) {
	rows, err := b.threadRows(ctx, room, root)
	if err != nil {
		return core.MessageItem{}, false, nil, err
	}
	items, err := b.items(ctx, room, rows)
	if err != nil {
		return core.MessageItem{}, false, nil, err
	}
	var parent core.MessageItem
	var found bool
	replies := make([]core.MessageItem, 0, len(items))
	for _, it := range items {
		if it.TS == rootTS {
			parent, found = it, true
			continue
		}
		replies = append(replies, it)
	}
	b.withMarkers(ctx, room, root, replies, "")
	b.threadOpened(room, root)
	return parent, found, replies, nil
}

// fetchThread is ThreadService.Fetch: the thread's replies from the
// server (/relations, m.thread, newest first, which is Backfill's order),
// stored through Backfill (which places them by origin time), then read
// from the store. Edits and reactions of replies come only from /sync.
func (b *Backend) fetchThread(alias, ts string) core.Msg {
	out := ui.ThreadRepliesLoadedMsg{ThreadTS: ts}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	room, root, err := b.threadRoot(ctx, alias, ts)
	if err != nil {
		return out // nil Replies: failure, the cached view stays
	}
	if b.client != nil {
		if err := b.fetchThreadEvents(ctx, room, root); err != nil {
			debuglog.Cache("matrix thread fetch: room=%s root=%s: %v", room, root, err)
			return out
		}
	}
	_, _, replies, err := b.threadItems(ctx, room, root, ts)
	if err != nil {
		return out
	}
	out.Replies = replies
	return out
}

func (b *Backend) fetchThreadEvents(ctx context.Context, room id.RoomID, root id.EventID) error {
	var evs []*event.Event
	req := &mautrix.ReqGetRelations{RelationType: event.RelThread, Dir: mautrix.DirectionBackward, Limit: serverPage}
	for range threadPages {
		resp, err := b.client.GetRelations(ctx, room, root, req)
		if err != nil {
			return err
		}
		evs = append(evs, resp.Chunk...)
		if resp.NextBatch == "" {
			break
		}
		req.From = resp.NextBatch
	}
	if _, stored, err := b.d.Store.GetEvent(ctx, room, root); err != nil {
		return err
	} else if !stored {
		e, err := b.client.GetEvent(ctx, room, root)
		if err != nil {
			return err
		}
		evs = append(evs, e) // the root is older than its replies
	}
	return b.backfill(ctx, room, evs, nil)
}

// threadCache is ThreadService.CacheRead: the stored root, then its
// replies; nil when the root is not stored.
func (b *Backend) threadCache(alias, ts string) []core.MessageItem {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	room, root, err := b.threadRoot(ctx, alias, ts)
	if err != nil {
		return nil
	}
	parent, ok, replies, err := b.threadItems(ctx, room, root, ts)
	if err != nil || !ok {
		return nil
	}
	return append([]core.MessageItem{parent}, replies...)
}

// threadLastRead is ThreadService.ThreadLastRead: the TS of the
// account's receipt in the thread.
func (b *Backend) threadLastRead(alias, ts string) string {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	room, root, err := b.threadRoot(ctx, alias, ts)
	if err != nil {
		return ""
	}
	r, _ := b.rooms.ThreadReceipt(room, root)
	return r.TS
}

// threadStat is one thread as the store has it.
type threadStat struct {
	room        id.RoomID
	root        id.EventID
	replies     int
	lastTS      string
	participant bool // the account replied
}

// listThreads is ThreadService.ListFetch: the threads the account is in
// (started or replied to), newest reply first, from the store. Every
// threadSweepEvery it first asks the server for each room's threads the
// account participated in (/threads): it keeps the reply counts and
// participation the server reports, which cover replies the store lacks,
// and stores each root the store lacks with its latest reply through
// Backfill, so threads older than the stored timeline are listed too.
func (b *Backend) listThreads(team string) core.Msg {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	snap := b.rooms.Snapshot()
	available := true
	if b.client != nil && b.sweepDue() {
		available = b.sweepThreads(ctx, snap) == nil
	}
	stats, err := b.threadStats(ctx)
	if err != nil {
		return ui.ThreadsListLoadedMsg{TeamID: team, SubscriptionsAvailable: available}
	}
	res := b.rooms.Resolver()
	var out []core.ThreadSummary
	for _, st := range stats {
		r, ok := b.rooms.RoomByID(st.room)
		if !ok || !slices.Contains(b.teamsOf(r.Alias), team) {
			continue
		}
		rootRow, ok, err := b.d.Store.GetEvent(ctx, st.room, st.root)
		b.mu.Lock()
		joined := b.threadJoined[st.root]
		b.mu.Unlock()
		if err != nil || !ok || (rootRow.Sender != b.self && !st.participant && !joined) {
			continue
		}
		parent, ok := b.item(ctx, st.room, rootRow, related{}, res)
		if !ok {
			parent = core.MessageItem{TS: rootRow.TS, UserID: res.UserAlias(rootRow.Sender), Text: deletedText}
		}
		lastBy := ""
		last, ok, _ := b.d.Store.GetEvent(ctx, st.room, b.eventAt(ctx, st.room, st.lastTS))
		if ok {
			lastBy = res.UserAlias(last.Sender)
		}
		b.mu.Lock()
		count := max(st.replies, b.threadCounts[st.root])
		b.mu.Unlock()
		read, _ := b.rooms.ThreadReceipt(st.room, st.root)
		readTS := max(read.TS, snap.ReadStates[r.Alias].LastReadTS)
		out = append(out, core.ThreadSummary{
			ChannelID:    r.Alias,
			ChannelName:  r.Name,
			ChannelType:  string(r.Kind),
			ThreadTS:     rootRow.TS,
			ParentUserID: parent.UserID,
			ParentText:   parent.Text,
			ParentTS:     rootRow.TS,
			ReplyCount:   count,
			LastReplyTS:  st.lastTS,
			LastReplyBy:  lastBy,
			Unread:       st.lastTS > readTS && last.Sender != b.self,
		})
		if len(out) == threadsListed {
			break
		}
	}
	return ui.ThreadsListLoadedMsg{TeamID: team, Summaries: out, SubscriptionsAvailable: available}
}

// eventAt is the event with TS ts in room, "" if none.
func (b *Backend) eventAt(ctx context.Context, room id.RoomID, ts string) id.EventID {
	ev, _, _ := b.d.IDs.Event(ctx, room, ts)
	return ev
}

// threadStats returns every stored thread, newest reply first.
func (b *Backend) threadStats(ctx context.Context) ([]threadStat, error) {
	rows, err := b.d.Store.DB.Query(ctx, `SELECT room_id,
			CASE WHEN thread_root != '' THEN thread_root ELSE relates_to_event END AS root,
			COUNT(*), MAX(ts), MAX(sender = $1)
		FROM slkmx_events
		WHERE redacted = 0 AND (thread_root != '' OR rel_type = 'm.thread')
			AND type IN ('m.room.message', 'm.room.encrypted', 'm.sticker')
			AND rel_type NOT IN ('m.replace', 'm.annotation')
		GROUP BY room_id, root ORDER BY MAX(ts) DESC`, b.self)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []threadStat
	for rows.Next() {
		var st threadStat
		if err := rows.Scan(&st.room, &st.root, &st.replies, &st.lastTS, &st.participant); err != nil {
			return nil, err
		}
		out = append(out, st)
	}
	return out, rows.Err()
}

// sweepDue reports whether a thread sweep is due, and claims it.
func (b *Backend) sweepDue() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if time.Since(b.threadSweep) < threadSweepEvery {
		return false
	}
	b.threadSweep = time.Now()
	return true
}

// sweepThreads asks the server for the threads the account participated
// in, in every room of the sidebar, a few rooms at a time.
func (b *Backend) sweepThreads(ctx context.Context, snap rooms.Snapshot) error {
	var roomIDs []id.RoomID
	for _, ch := range snap.Channels {
		if r, ok := b.rooms.Room(ch.ID); ok {
			roomIDs = append(roomIDs, r.ID)
		}
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var first error
	sem := make(chan struct{}, 4)
	for _, room := range roomIDs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			if err := b.sweepRoom(ctx, room); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return first
}

func (b *Backend) sweepRoom(ctx context.Context, room id.RoomID) error {
	resp, err := b.client.GetThreads(ctx, room, &mautrix.ReqGetThreads{Include: mautrix.ThreadIncludeParticipated, Limit: 20})
	if err != nil {
		return err
	}
	for _, root := range resp.Chunk {
		var latest *event.Event
		if agg := root.Unsigned.Relations; agg != nil && agg.Thread != nil {
			b.mu.Lock()
			b.threadCounts[root.ID] = agg.Thread.Count
			b.threadJoined[root.ID] = true // include=participated
			b.mu.Unlock()
			latest = agg.Thread.LatestEvent
		}
		_, stored, err := b.d.Store.GetEvent(ctx, room, root.ID)
		if err != nil {
			return err
		}
		if stored {
			continue
		}
		// One thread per call, newest first: Backfill places events
		// after one that went below the stored timeline below it too.
		evs := []*event.Event{root}
		if latest != nil && latest.ID != "" {
			evs = []*event.Event{latest, root}
		}
		if err := b.backfill(ctx, room, evs, nil); err != nil {
			return err
		}
	}
	return nil
}

// searchChannel is SearchService.SearchChannel: the store's full-text
// matches among the channel's own rows (an edit counts for the message it
// edits; thread replies are not in the channel pane), newest first.
func (b *Backend) searchChannel(alias, query string) core.Msg {
	out := ui.ChannelSearchResultsMsg{ChannelID: alias, Query: query, Terms: strings.Fields(text.Fold(query))}
	ctx, cancel, room, err := b.roomCtx(alias)
	if err != nil {
		out.Err = err
		return out
	}
	defer cancel()
	evs, err := b.d.Store.SearchRoom(ctx, room, query, 500)
	if err != nil {
		out.Err = err
		return out
	}
	seen := map[string]bool{}
	for _, e := range evs {
		if event.RelationType(e.RelType) == event.RelReplace {
			target, ok, err := b.d.Store.GetEvent(ctx, room, e.RelatesTo)
			if err != nil || !ok || target.Sender != e.Sender {
				continue
			}
			e = target
		}
		if !isMainRow(e) || e.Redacted || seen[e.TS] {
			continue
		}
		seen[e.TS] = true
		out.TSes = append(out.TSes, e.TS)
	}
	slices.SortFunc(out.TSes, func(x, y string) int { return strings.Compare(y, x) })
	return out
}

// readStates is UnreadService.ChannelReadStates: the room model's, with
// rooms read in this process only (markReadLocal) read up to where they
// were marked, until someone else posts after that.
func (b *Backend) readStates() map[string]core.ReadState {
	states := b.rooms.Snapshot().ReadStates
	b.mu.Lock()
	local := maps.Clone(b.localRead)
	b.mu.Unlock()
	for alias, ts := range local {
		rs, ok := states[alias]
		room, known := b.rooms.Room(alias)
		if !ok || !known {
			continue
		}
		rs.LastReadTS = max(rs.LastReadTS, ts)
		if rs.HasUnread {
			newest, err := b.d.Store.LatestUnreadCandidate(context.Background(), room.ID, b.self)
			if err == nil && newest <= ts {
				rs.HasUnread, rs.MentionCount = false, 0
			}
		}
		states[alias] = rs
	}
	return states
}

// markReadLocal is read-only mode's ChannelService.MarkRead: the room
// reads as read up to ts in this process, and the server is not told.
func (b *Backend) markReadLocal(ch ids.ChannelID, ts ids.MessageTS) core.Msg {
	b.mu.Lock()
	b.localRead[string(ch)] = max(b.localRead[string(ch)], string(ts))
	b.mu.Unlock()
	return ui.ChannelMarkedReadMsg{ChannelID: string(ch)}
}

// unreadWorkspaces is UnreadService.UnreadWorkspaces: the workspaces
// with an unread room, in rail order.
func (b *Backend) unreadWorkspaces() []string {
	states := b.readStates()
	list, teams := b.current()
	var out []string
	for i, ws := range list {
		if slices.ContainsFunc(ws.Channels, func(c sidebar.ChannelItem) bool { return states[c.ID].HasUnread }) {
			out = append(out, teams[i])
		}
	}
	return out
}

// avatarPalette and the tile below are internal/demo's renderAvatar,
// which is unexported there.
var avatarPalette = [][3]uint8{
	{0xe0, 0x6c, 0x75}, {0xd1, 0x9a, 0x66}, {0x98, 0xc3, 0x79}, {0x56, 0xb6, 0xc2},
	{0x61, 0xaf, 0xef}, {0xc6, 0x78, 0xdd}, {0xbe, 0x50, 0x46}, {0x2b, 0x8a, 0x6e},
}

// avatar is AvatarService.Avatar: the user's initials on a colour picked
// from their alias, as SGR text (a 4x2-cell half-block image is too small
// for letters). Matrix avatar images are WP-Media's.
func (b *Backend) avatar(alias string) string {
	user, ok := b.rooms.Resolver().MXID(alias)
	if !ok {
		return ""
	}
	name := b.rooms.DisplayName("", user)
	h := fnv.New32a()
	h.Write([]byte(alias))
	c := avatarPalette[h.Sum32()%uint32(len(avatarPalette))]
	top := fmt.Sprintf("\x1b[1;38;2;255;255;255;48;2;%d;%d;%dm %s \x1b[0m", c[0], c[1], c[2], initials(name))
	bottom := fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", c[0], c[1], c[2], strings.Repeat("▀", avatar.AvatarCols))
	return top + "\n" + bottom
}

// initials returns exactly two cells: the first letters of the first two
// words, or the first two letters of a single word.
func initials(name string) string {
	words := strings.Fields(strings.TrimPrefix(name, "@"))
	var rs []rune
	switch len(words) {
	case 0:
		return "??"
	case 1:
		rs = []rune(words[0])
	default:
		rs = []rune{[]rune(words[0])[0], []rune(words[1])[0]}
	}
	if len(rs) > 2 {
		rs = rs[:2]
	}
	for len(rs) < 2 {
		rs = append(rs, ' ')
	}
	for i := range rs {
		rs[i] = unicode.ToUpper(rs[i])
	}
	return string(rs)
}
