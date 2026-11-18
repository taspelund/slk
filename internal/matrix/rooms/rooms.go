// Package rooms is the account's rooms as slk sees them, and the one
// writer of the account database's room and timeline tables.
//
// Every event that reaches the backend, live from /sync or fetched by
// paging back, goes through a Model: it mints the event's TS and the
// aliases of everyone involved (ident), converts message bodies (mxfmt),
// stores the event (store), updates the room's row (name, kind, sections,
// counts) and receipts, and reports what changed. Readers (internal/matrix/svc)
// only read the store and the Model's snapshots.
//
// Room to sidebar mapping:
//
//   - a room in m.direct with one other member is a "dm", with more a
//     "group_dm"; otherwise join rule "public" is a "channel" and anything
//     else "private"
//   - names: m.room.name, else the canonical alias, else names computed
//     from the heroes (the spec's room-name algorithm), for DMs the other
//     member's display name
//   - workspaces (Workspaces): Direct Messages holds the DMs, then the
//     rooms in no space are one workspace per server (the account's own
//     first), then each top-level space is a workspace whose sub-spaces
//     are sidebar sections. Snapshot's single sidebar
//     (Channels, Sections) puts a room in the section of its first parent
//     space (by m.space.child "order", then by name), rooms in no space in
//     "Rooms", DMs in "Direct Messages". Spaces themselves are not
//     sidebar rows
//   - invites and rooms that were left are not sidebar rows yet (WP-UI)
//
// Read state, per room alias:
//
//   - LastReadTS is the TS of the account's own read receipt (main
//     timeline; threaded receipts are kept per thread)
//   - HasUnread: a message event (not a notice, not the account's own,
//     not a relation such as an edit or reaction) after the receipt
//   - MentionCount: the room's highlight_count from /sync
//   - per-thread unread state is computed from threaded receipts, because
//     the homeserver does not send per-thread counts (MSC3773 is off)
//
// Details of the above: the receipt is the later of the unthreaded and
// "main" receipts (m.read or m.read.private; receipts never move back);
// the newest stored main-timeline event the account sent counts as read;
// thread replies and still-encrypted thread replies do not make the room
// unread; m.marked_unread makes it unread; and when the receipt is on an
// event the store does not hold (older than the stored window), the
// room's notification_count decides until that event is backfilled.
//
// What is stored (slkmx_events), for readers:
//
//   - m.room.message, m.sticker, m.reaction and still-encrypted
//     m.room.encrypted events, each with its TS; content_json is the
//     event's own content (decrypted when decrypted = 1). Other events
//     (state, redactions) are applied, not stored.
//   - An edit is its own row (rel_type m.replace, relates_to_event the
//     original, body ""). The original keeps its content_json and its body
//     becomes the text of its latest valid edit (an unredacted m.replace
//     from the same sender); store.Relations lists the edits, so a reader
//     takes m.new_content from the newest valid one.
//   - A reaction is a row with rel_type m.annotation; its key is in
//     content_json.
//   - A thread reply has thread_root (and relates_to_event) set to the
//     root; edits and reactions never have thread_root set.
//   - A redacted event keeps its row with redacted = 1, content_json {}
//     and body "".
package rooms

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/text"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// Model holds the account's rooms in memory, backed by the store. It is
// safe for concurrent use: Apply and Backfill are serialized; snapshots and
// lookups may run alongside them.
//
// Writers work on copies of the rooms they change and publish them when
// done, so readers never wait for a writer's I/O and never see half a
// batch.
type Model struct {
	st   *store.Store
	ids  *ident.Mapper
	ss   *sqlstatestore.SQLStateStore
	self id.UserID
	now  func() time.Time

	// wmu serializes Apply, ApplyDecrypted and Backfill.
	wmu sync.Mutex

	// mu guards the fields below. Each is replaced, never modified, once
	// published, except rooms, which is only written under mu.
	mu      sync.RWMutex
	rooms   map[id.RoomID]*room
	byAlias map[string]id.RoomID
	// edges are the space edges: child room -> space -> order.
	edges map[id.RoomID]map[id.RoomID]string
	// direct is the latest m.direct (room -> users), nil until one
	// arrives after New.
	direct map[id.RoomID][]id.UserID
	// global is every known user's global display name.
	global map[id.UserID]string

	aliasMu   sync.Mutex
	aliasOf   map[id.UserID]string
	aliasUser map[string]id.UserID
}

// New loads the account's rooms from st. self is the account's user ID.
//
// Members are read from mautrix's state store on st.DB, which the model
// also writes (the session's syncer writes the same rows). Everything else
// a room's name and kind are computed from (m.room.name, the sync
// summary's heroes and member counts, the join rule, the m.direct users)
// is in the room's row, so a restarted model computes what the old one
// did. Rows from before those columns existed (store schema v1) restore
// what they can (see loadLegacyInputs) until a batch touches them.
func New(ctx context.Context, st *store.Store, ids *ident.Mapper, self id.UserID) (*Model, error) {
	ss := sqlstatestore.NewSQLStateStore(st.DB, nil, false)
	if err := ss.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("rooms: state store: %w", err)
	}
	m := &Model{
		st: st, ids: ids, ss: ss, self: self, now: time.Now,
		rooms: map[id.RoomID]*room{}, byAlias: map[string]id.RoomID{},
		edges:   map[id.RoomID]map[id.RoomID]string{},
		aliasOf: map[id.UserID]string{}, aliasUser: map[string]id.UserID{},
	}
	if err := m.load(ctx); err != nil {
		return nil, fmt.Errorf("rooms: loading: %w", err)
	}
	return m, nil
}

func (m *Model) load(ctx context.Context) error {
	rows, err := m.st.ListRooms(ctx)
	if err != nil {
		return err
	}
	edges, err := m.st.SpaceParents(ctx)
	if err != nil {
		return err
	}
	for _, e := range edges {
		if m.edges[e.RoomID] == nil {
			m.edges[e.RoomID] = map[id.RoomID]string{}
		}
		m.edges[e.RoomID][e.SpaceID] = e.Order
	}
	receipts, err := m.st.UserReceipts(ctx, m.self)
	if err != nil {
		return err
	}
	for _, row := range rows {
		alias, err := m.roomAlias(ctx, row.ID)
		if err != nil {
			return err
		}
		r := newRoom(row.ID, alias)
		r.row = row
		members, err := m.ss.GetRoomMembers(ctx, row.ID)
		if err != nil {
			return err
		}
		for u, mem := range members {
			if _, err := m.userAlias(ctx, u); err != nil {
				return err
			}
			r.members[u] = member{name: mem.Displayname, membership: mem.Membership}
		}
		if row.InputsSaved {
			r.loadInputs()
		} else if err := m.loadLegacyInputs(ctx, r); err != nil {
			return err
		}
		if r.candTS, err = m.st.LatestUnreadCandidate(ctx, row.ID, m.self); err != nil {
			return err
		}
		if r.ownTS, err = m.st.LatestSentBy(ctx, row.ID, m.self); err != nil {
			return err
		}
		if row.DMUserID != "" {
			if _, err := m.userAlias(ctx, row.DMUserID); err != nil {
				return err
			}
		}
		m.rooms[row.ID] = r
		m.byAlias[alias] = row.ID
	}
	for _, rec := range receipts {
		if r := m.rooms[rec.RoomID]; r != nil {
			r.receipts[rec.ThreadID] = rec
		}
	}
	m.global = buildGlobal(m.rooms)
	dn := func(rid id.RoomID, user id.UserID) string { return displayName(m.rooms[rid], m.global, user) }
	for _, r := range m.rooms {
		if r.row.InputsSaved {
			continue
		}
		kind, dmUser := r.kind(m.self)
		if computed := r.name(m.self, kind, dmUser, dn); r.row.Name != "" && r.row.Name != computed {
			name := r.row.Name
			r.explicitName = &name
		}
	}
	return nil
}

// loadLegacyInputs restores what it can of the name and kind inputs of a
// row written before they were stored (schema v1): the join rule from
// mautrix's state store, m.direct from the stored kind. load then treats
// a stored name that differs from the computed one as the room's
// m.room.name. The row has its inputs from the next batch that touches
// the room.
func (m *Model) loadLegacyInputs(ctx context.Context, r *room) error {
	row := r.row
	if jr, err := m.ss.GetJoinRules(ctx, row.ID); err != nil {
		return err
	} else if jr != nil {
		r.joinRule = jr.JoinRule
	} else if row.Kind == store.RoomChannel {
		r.joinRule = event.JoinRulePublic
	}
	switch row.Kind {
	case store.RoomDM:
		r.direct = true
		if row.DMUserID != "" {
			r.directUsers = []id.UserID{row.DMUserID}
		}
	case store.RoomGroupDM:
		r.direct, r.minOthers = true, 2
	}
	if row.JoinState == store.Invited && row.Kind == store.RoomDM {
		r.direct, r.directUsers, r.inviteDirect, r.inviter = false, nil, true, row.DMUserID
	}
	return nil
}

// roomAlias returns room's alias, minting it if needed.
func (m *Model) roomAlias(ctx context.Context, room id.RoomID) (string, error) {
	return m.ids.RoomAlias(ctx, room)
}

// userAlias returns user's alias, minting it if needed, and caches it for
// the Resolver.
func (m *Model) userAlias(ctx context.Context, user id.UserID) (string, error) {
	m.aliasMu.Lock()
	a, ok := m.aliasOf[user]
	m.aliasMu.Unlock()
	if ok {
		return a, nil
	}
	a, err := m.ids.UserAlias(ctx, user)
	if err != nil {
		return "", err
	}
	m.aliasMu.Lock()
	m.aliasOf[user], m.aliasUser[a] = a, user
	m.aliasMu.Unlock()
	return a, nil
}

// cachedUserAlias returns user's alias if the model has seen it.
func (m *Model) cachedUserAlias(user id.UserID) (string, bool) {
	m.aliasMu.Lock()
	defer m.aliasMu.Unlock()
	a, ok := m.aliasOf[user]
	return a, ok
}

// Apply takes in one /sync response: room state, timeline events, receipts,
// unread counts, account data (m.direct, m.marked_unread) and membership
// changes. A limited timeline records a gap (store.AddGap) before the first
// new event. Events that are still encrypted are stored as such and
// replaced when a LateDecrypted delivers them.
func (m *Model) Apply(ctx context.Context, b session.SyncBatch) (Changes, error) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	if b.Resp == nil {
		return Changes{}, nil
	}
	return m.apply(ctx, b.Resp)
}

// ApplyDecrypted replaces stored encrypted events with their decrypted form.
func (m *Model) ApplyDecrypted(ctx context.Context, d session.LateDecrypted) (Changes, error) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	b := m.newBatch(ctx)
	r, err := b.room(d.Room)
	if err != nil {
		return Changes{}, err
	}
	for _, evt := range d.Events {
		if evt.ID == "" || evt.StateKey != nil {
			continue
		}
		switch evt.Type.Type {
		case typeMessage, typeSticker, typeEncrypted, typeReaction:
		default:
			continue
		}
		ts, ok, err := m.ids.TS(ctx, d.Room, evt.ID)
		if err != nil {
			return Changes{}, err
		}
		if !ok {
			if ts, err = b.liveTS(d.Room, evt); err != nil {
				return Changes{}, err
			}
		}
		if err := b.storeEvent(r, evt, ts, true); err != nil {
			return Changes{}, fmt.Errorf("rooms: decrypted %s: %w", evt.ID, err)
		}
	}
	return b.finish()
}

// Backfill takes in events fetched from the server outside /sync: pages
// back (newest first, as /messages returns them with dir=b), a thread's
// replies from /relations (newest first, the root after them), or a
// /context window (events_after reversed, the event, events_before).
// state is the response's state (member events for the senders).
//
// Each event is placed by where it belongs: one older than the room's
// oldest TS gets a TS before it (ident.TSForBackfill), and so does every
// event after it in events, which are older still; any other event is
// placed among the stored ones by its origin time (ident.TSForInsert).
// Events already mapped keep their TS. Changes report them with Live
// false. Events filling a gap go through FillGap instead, which orders
// them by the page rather than by origin time.
//
// Member events in state name senders the model has no member event for;
// they never replace a newer one. State events among events are history
// and do not change the room's state.
func (m *Model) Backfill(ctx context.Context, room id.RoomID, events []*event.Event, state []*event.Event) (Changes, error) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	b := m.newBatch(ctx)
	r, err := b.room(room)
	if err != nil {
		return Changes{}, err
	}
	if err := b.backfillState(r, state); err != nil {
		return Changes{}, err
	}
	oldest, _, haveBounds, err := m.st.RoomBounds(ctx, room)
	if err != nil {
		return Changes{}, err
	}
	below := false // an event of this page went before the oldest
	for _, evt := range events {
		if !storable(evt) {
			continue
		}
		origin := time.UnixMilli(evt.Timestamp)
		ts, mapped, err := m.ids.TS(ctx, room, evt.ID)
		switch {
		case err != nil:
		case mapped:
			below = below || (haveBounds && ts <= oldest)
		case !haveBounds || below || naturalTS(origin) < oldest:
			ts, err = m.ids.TSForBackfill(ctx, room, evt.ID, origin)
			below = true
		default:
			ts, err = m.ids.TSForInsert(ctx, room, evt.ID, origin)
		}
		if err != nil {
			return Changes{}, err
		}
		if err := b.storeEvent(r, evt, ts, false); err != nil {
			return Changes{}, fmt.Errorf("rooms: backfilled %s: %w", evt.ID, err)
		}
	}
	return b.finish()
}

// FillGap takes in one page of events fetched for gap gapID of room
// (store.Gaps): /messages from the gap's prev_batch with dir=b, so newest
// first, with nextPrevBatch the response's end ("" when it has none) and
// state its state. Each new event gets a TS strictly between the event
// after it (the gap's after_ts for the first) and the newest TS mapped
// before that (ident.TSForInsertBetween), so the page's order is the
// timeline order whatever the origin times; only when no TS is free
// there is an event placed by origin time (ident.TSForInsert). Events
// already mapped keep their TS. Changes report the new events with Live
// false.
//
// Then the gap is removed when the page completes it, otherwise moved:
// its prev_batch becomes nextPrevBatch and its after_ts the page's oldest
// TS, for the next page. A caller sets complete when it knows the gap is
// closed (it fetched up to a known event, or the server's history ended);
// FillGap also treats the page as complete when nextPrevBatch is "" or
// when the page holds an event that was already stored before the gap's
// after_ts (the page reached the stored timeline below the gap). A
// caller that pages /messages from prev_batch can therefore pass
// complete = false and let FillGap decide.
func (m *Model) FillGap(ctx context.Context, room id.RoomID, gapID int64, events, state []*event.Event, nextPrevBatch string, complete bool) (Changes, error) {
	m.wmu.Lock()
	defer m.wmu.Unlock()
	g, ok, err := m.st.Gap(ctx, gapID)
	if err != nil {
		return Changes{}, err
	}
	if !ok || g.RoomID != room {
		return Changes{}, fmt.Errorf("rooms: no gap %d in %s", gapID, room)
	}
	b := m.newBatch(ctx)
	r, err := b.room(room)
	if err != nil {
		return Changes{}, err
	}
	if err := b.backfillState(r, state); err != nil {
		return Changes{}, err
	}
	before := g.AfterTS
	for _, evt := range events {
		if !storable(evt) {
			continue
		}
		origin := time.UnixMilli(evt.Timestamp)
		ts, mapped, err := m.ids.TS(ctx, room, evt.ID)
		if err != nil {
			return Changes{}, err
		}
		if mapped {
			if _, stored, err := m.st.GetEvent(ctx, room, evt.ID); err != nil {
				return Changes{}, err
			} else if stored && ts < g.AfterTS {
				complete = true
			}
			before = min(before, ts)
		} else {
			after, _, err := m.st.TSBefore(ctx, room, before)
			if err != nil {
				return Changes{}, err
			}
			ts, err = m.ids.TSForInsertBetween(ctx, room, evt.ID, origin, after, before)
			switch {
			case errors.Is(err, ident.ErrNoRoom):
				ts, err = m.ids.TSForInsert(ctx, room, evt.ID, origin)
			case err == nil:
				before = ts
			}
			if err != nil {
				return Changes{}, err
			}
		}
		if err := b.storeEvent(r, evt, ts, false); err != nil {
			return Changes{}, fmt.Errorf("rooms: gap event %s: %w", evt.ID, err)
		}
	}
	if complete || nextPrevBatch == "" {
		err = m.st.RemoveGap(ctx, gapID)
	} else {
		err = m.st.UpdateGap(ctx, gapID, nextPrevBatch, before)
	}
	if err != nil {
		return Changes{}, err
	}
	return b.finish()
}

// storable reports whether evt is a timeline event the model stores.
func storable(evt *event.Event) bool {
	if evt.ID == "" || evt.StateKey != nil {
		return false
	}
	switch evt.Type.Type {
	case typeMessage, typeSticker, typeEncrypted, typeReaction:
		return true
	}
	return false
}

// naturalTS is the TS of origin without a tiebreak, as ident formats it.
func naturalTS(origin time.Time) string {
	ms := max(origin.UnixMilli(), 0)
	return fmt.Sprintf("%010d.%06d", ms/1000, ms%1000*1000)
}

// backfillState applies the member events of a fetched page's state for
// senders the model has no member event for.
func (b *batch) backfillState(r *room, state []*event.Event) error {
	for _, evt := range state {
		if evt.StateKey == nil || evt.Type.Type != event.StateMember.Type {
			continue
		}
		if _, ok := r.members[id.UserID(*evt.StateKey)]; ok {
			continue
		}
		if err := b.applyMember(r, id.UserID(*evt.StateKey), evt); err != nil {
			return err
		}
	}
	return nil
}

// Changes is what one Apply, ApplyDecrypted or Backfill changed. svc turns
// it into ui messages (WP-Live).
type Changes struct {
	// Timeline lists the stored events that changed, in the order they
	// were applied.
	Timeline []TimelineChange
	// Rooms lists rooms whose sidebar row (name, kind, section, topic,
	// membership) changed, spaces included; Joined and Left are rooms the
	// account joined or left in this batch. A room seen for the first time
	// (every room of the first sync) is in Rooms, and in Joined when
	// joined.
	Rooms        []id.RoomID
	Joined, Left []id.RoomID
	// ReadStates lists rooms whose read state changed. OwnReceipts lists
	// the account's own receipts that moved (from another device or this
	// one).
	ReadStates  []id.RoomID
	OwnReceipts []Receipt
	// Typing maps a room to the users typing there now (complete list).
	Typing map[id.RoomID][]id.UserID
	// Names lists users whose display name changed.
	Names []id.UserID
}

// ChangeKind says what happened to an event.
type ChangeKind int

const (
	// Added is a message (or other displayed event) that was not stored
	// before.
	Added ChangeKind = iota + 1
	// Edited is a message whose content changed (an m.replace arrived, or
	// it was decrypted late). Event is the edited message, not the edit.
	Edited
	// Redacted is a message that was redacted.
	Redacted
	// ReactionAdded and ReactionRemoved are an m.annotation added to or
	// redacted from Event (the reacted-to message); Key is the reaction
	// and Sender who reacted.
	ReactionAdded
	ReactionRemoved
)

// TimelineChange is one change to a stored event.
type TimelineChange struct {
	Kind   ChangeKind
	Room   id.RoomID
	Event  id.EventID
	TS     string
	Thread id.EventID // thread root, "" for the main timeline
	Key    string     // ReactionAdded/Removed: the reaction key
	Sender id.UserID  // ReactionAdded/Removed: who reacted
	// Live is true for events from /sync, false for backfill.
	Live bool
}

// Receipt is the account's own read receipt in a room or thread.
type Receipt struct {
	Room   id.RoomID
	Thread id.EventID // "" for the main timeline
	Event  id.EventID
	TS     string
}

// Room is one room as the backend needs it.
type Room struct {
	ID        id.RoomID
	Alias     string // ident alias, "C…"
	Name      string
	Kind      store.RoomKind
	Encrypted bool
	DMUser    id.UserID
	// Section is the sidebar section ID (see Snapshot), "" for a room
	// that is not a sidebar row (a space, an invite, a room left).
	Section string
}

// Snapshot is the sidebar's view of the account. Every call returns fresh
// maps and slices that the caller may hand to the UI.
type Snapshot struct {
	Channels     []sidebar.ChannelItem
	Finder       []core.ChannelFinderItem
	ChannelNames map[string]string // room alias -> name
	UserNames    map[string]string // user alias -> display name
	ReadStates   map[string]core.ReadState
	// Sections are the sidebar sections in order, for a
	// sidebar.SectionsProvider.
	Sections []sidebar.SectionMeta
}

// Snapshot returns the current sidebar view.
//
// Sections are the spaces that hold at least one sidebar room (type
// "standard", ID the space's room alias), by name, then "Rooms" (type
// "channels") and "Direct Messages" (type "direct_messages"). Channels and
// Finder list the joined rooms that are not spaces, by section, then name
// (case- and accent-folded, as the sidebar sorts within a section), then
// alias; SectionOrder is the section's position from 1. ChannelNames
// holds every room the model knows, UserNames every user with a member
// event, both keyed by alias. Each call builds new maps and slices.
func (m *Model) Snapshot() Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	get := func(rid id.RoomID) *room { return m.rooms[rid] }

	type entry struct {
		r       *room
		section string
	}
	var entries []entry
	spaces := map[string]*room{}
	for _, r := range m.rooms {
		if !r.sidebar() {
			continue
		}
		sec := sectionOf(r, get, m.edges)
		if sec != sectionRooms && sec != sectionDMs {
			spaces[sec] = m.rooms[m.byAlias[sec]]
		}
		entries = append(entries, entry{r, sec})
	}
	var spaceIDs []string
	for sec := range spaces {
		spaceIDs = append(spaceIDs, sec)
	}
	slices.SortFunc(spaceIDs, func(a, b string) int {
		if na, nb := text.Fold(spaces[a].row.Name), text.Fold(spaces[b].row.Name); na != nb {
			return cmpString(na, nb)
		}
		return cmpString(a, b)
	})
	snap := Snapshot{
		ChannelNames: map[string]string{},
		UserNames:    map[string]string{},
		ReadStates:   map[string]core.ReadState{},
	}
	rank := map[string]int{}
	for _, sec := range spaceIDs {
		rank[sec] = len(snap.Sections)
		snap.Sections = append(snap.Sections, sidebar.SectionMeta{ID: sec, Name: spaces[sec].row.Name, Type: "standard"})
	}
	rank[sectionRooms] = len(snap.Sections)
	snap.Sections = append(snap.Sections, sidebar.SectionMeta{ID: sectionRooms, Name: "Rooms", Type: "channels"})
	rank[sectionDMs] = len(snap.Sections)
	snap.Sections = append(snap.Sections, sidebar.SectionMeta{ID: sectionDMs, Name: "Direct Messages", Type: "direct_messages"})

	slices.SortFunc(entries, func(a, b entry) int {
		if rank[a.section] != rank[b.section] {
			return rank[a.section] - rank[b.section]
		}
		if na, nb := text.Fold(a.r.row.Name), text.Fold(b.r.row.Name); na != nb {
			return cmpString(na, nb)
		}
		return cmpString(a.r.alias, b.r.alias)
	})
	for _, e := range entries {
		r := e.r
		dm := ""
		if r.row.DMUserID != "" {
			dm, _ = m.cachedUserAlias(r.row.DMUserID)
		}
		snap.Channels = append(snap.Channels, sidebar.ChannelItem{
			ID: r.alias, Name: r.row.Name, Type: string(r.row.Kind), Section: e.section,
			SectionOrder: rank[e.section] + 1, DMUserID: dm,
		})
		snap.Finder = append(snap.Finder, core.ChannelFinderItem{
			ID: r.alias, Name: r.row.Name, Type: string(r.row.Kind), Joined: true, DMUserID: dm,
		})
		snap.ReadStates[r.alias] = r.readState()
	}
	for _, r := range m.rooms {
		snap.ChannelNames[r.alias] = r.row.Name
		for u := range r.members {
			if a, ok := m.cachedUserAlias(u); ok {
				if _, done := snap.UserNames[a]; !done {
					snap.UserNames[a] = displayName(nil, m.global, u)
				}
			}
		}
	}
	return snap
}

func cmpString(a, b string) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// Room returns the room an alias names.
func (m *Model) Room(alias string) (Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rid, ok := m.byAlias[alias]
	if !ok {
		return Room{}, false
	}
	return m.roomLocked(rid)
}

// RoomByID returns a room by its Matrix ID.
func (m *Model) RoomByID(room id.RoomID) (Room, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.roomLocked(room)
}

// roomLocked builds the Room for rid; m.mu is held. Section is the
// sidebar section ID, "" for a room that is not a sidebar row.
func (m *Model) roomLocked(rid id.RoomID) (Room, bool) {
	r := m.rooms[rid]
	if r == nil {
		return Room{}, false
	}
	return Room{
		ID: rid, Alias: r.alias, Name: r.row.Name, Kind: r.row.Kind, Encrypted: r.row.Encrypted,
		DMUser:  r.row.DMUserID,
		Section: sidebarSection(r, func(s id.RoomID) *room { return m.rooms[s] }, m.edges),
	}, true
}

// DisplayName returns a user's display name in room ("" room for the
// global one), falling back to the localpart.
//
// The name in a room is the user's member event's displayname there; the
// global one is the first non-empty displayname of their member events
// across rooms (by room ID), since the model does not fetch profiles.
func (m *Model) DisplayName(rid id.RoomID, user id.UserID) string {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var r *room
	if rid != "" {
		r = m.rooms[rid]
	}
	return displayName(r, m.global, user)
}

// Resolver returns an mxfmt.Resolver over the model's aliases and names.
// Its methods do not block on I/O beyond minting a new alias.
func (m *Model) Resolver() mxfmt.Resolver {
	return resolver{m}
}

// ThreadReceipt returns the account's receipt in a thread: the later of
// its receipt for that thread and its unthreaded receipt (which covers
// threads too). Thread is set to thread either way.
func (m *Model) ThreadReceipt(room id.RoomID, thread id.EventID) (Receipt, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r := m.rooms[room]
	if r == nil || thread == "" {
		return Receipt{}, false
	}
	t, okT := r.receipts[string(thread)]
	u, okU := r.receipts[""]
	var rec store.Receipt
	switch {
	case okT && okU:
		rec = t
		if u.TS > t.TS {
			rec = u
		}
	case okT:
		rec = t
	case okU:
		rec = u
	default:
		return Receipt{}, false
	}
	out := receiptOf(rec)
	out.Thread = thread
	return out, true
}
