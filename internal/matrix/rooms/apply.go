package rooms

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/store"
)

// maxFuture caps how far past now a live event's origin_server_ts may be
// when its TS is minted. A live TS sorts after the room's newest, so one
// far-future origin would push every later event in the room into the
// future with it.
const maxFuture = 5 * time.Minute

// Event and content types the model reads, as strings: events built from
// JSON have a Type whose Class is unknown, so event.Type values do not
// compare equal to them.
const (
	typeMessage    = "m.room.message"
	typeSticker    = "m.sticker"
	typeEncrypted  = "m.room.encrypted"
	typeReaction   = "m.reaction"
	typeRedaction  = "m.room.redaction"
	relReplace     = "m.replace"
	relAnnotation  = "m.annotation"
	relThread      = "m.thread"
	markedUnread   = "m.marked_unread"
	markedUnreadV0 = "com.famedly.marked_unread"
)

// msgContent is what the model reads of a timeline event's content.
type msgContent struct {
	MsgType   string `json:"msgtype"`
	Body      string `json:"body"`
	RelatesTo *struct {
		RelType   string     `json:"rel_type"`
		EventID   id.EventID `json:"event_id"`
		Key       string     `json:"key"`
		InReplyTo *struct {
			EventID id.EventID `json:"event_id"`
		} `json:"m.in_reply_to"`
	} `json:"m.relates_to"`
	NewContent *msgContent `json:"m.new_content"`
	Redacts    id.EventID  `json:"redacts"`
}

func (c *msgContent) relType() string {
	if c.RelatesTo == nil {
		return ""
	}
	return c.RelatesTo.RelType
}

// text is the plain text of a message for the full-text index, without a
// reply fallback.
func (c *msgContent) text() string {
	if c.RelatesTo != nil && c.RelatesTo.InReplyTo != nil {
		return event.TrimReplyFallbackText(c.Body)
	}
	return c.Body
}

// rawContent is evt's content JSON.
func rawContent(evt *event.Event) json.RawMessage {
	if len(evt.Content.VeryRaw) > 0 {
		return evt.Content.VeryRaw
	}
	b, err := json.Marshal(&evt.Content)
	if err != nil {
		return nil
	}
	return b
}

func parseContent(evt *event.Event, v any) {
	if raw := rawContent(evt); len(raw) > 0 {
		_ = json.Unmarshal(raw, v)
	}
}

// batch is one Apply, ApplyDecrypted or Backfill in progress. It works on
// clones of the rooms it touches and publishes them in commit.
type batch struct {
	m   *Model
	ctx context.Context
	ch  Changes

	rooms     map[id.RoomID]*room // clones and new rooms
	wasJoined map[id.RoomID]store.JoinState
	edges     map[id.RoomID]map[id.RoomID]string // nil until changed
	direct    map[id.RoomID][]id.UserID          // m.direct, nil until it arrives
	timeline  map[id.RoomID]bool                 // rooms whose stored events changed
	names     map[id.UserID]bool
	members   bool // some member changed
}

func (m *Model) newBatch(ctx context.Context) *batch {
	return &batch{
		m: m, ctx: ctx,
		rooms:     map[id.RoomID]*room{},
		wasJoined: map[id.RoomID]store.JoinState{},
		timeline:  map[id.RoomID]bool{},
		names:     map[id.UserID]bool{},
	}
}

// room returns room rid for writing, cloning the published one or
// creating it.
func (b *batch) room(rid id.RoomID) (*room, error) {
	if r, ok := b.rooms[rid]; ok {
		return r, nil
	}
	b.m.mu.RLock()
	pub := b.m.rooms[rid]
	b.m.mu.RUnlock()
	var r *room
	if pub != nil {
		r = pub.clone()
		b.wasJoined[rid] = pub.row.JoinState
	} else {
		alias, err := b.m.roomAlias(b.ctx, rid)
		if err != nil {
			return nil, err
		}
		r = newRoom(rid, alias)
		r.row.JoinState = ""
		b.wasJoined[rid] = ""
		b.m.mu.RLock()
		if d := b.m.direct; d != nil {
			r.direct, r.directUsers = len(d[rid]) > 0, slices.Clone(d[rid])
		}
		b.m.mu.RUnlock()
	}
	b.rooms[rid] = r
	return r, nil
}

// edgesForWrite returns the space edges for modification.
func (b *batch) edgesForWrite() map[id.RoomID]map[id.RoomID]string {
	if b.edges == nil {
		b.m.mu.RLock()
		b.edges = make(map[id.RoomID]map[id.RoomID]string, len(b.m.edges))
		for k, v := range b.m.edges {
			b.edges[k] = maps.Clone(v)
		}
		b.m.mu.RUnlock()
	}
	return b.edges
}

func (b *batch) currentEdges() map[id.RoomID]map[id.RoomID]string {
	if b.edges != nil {
		return b.edges
	}
	b.m.mu.RLock()
	defer b.m.mu.RUnlock()
	return b.m.edges
}

// Apply implements the contract in rooms.go.
func (m *Model) apply(ctx context.Context, resp *mautrix.RespSync) (Changes, error) {
	b := m.newBatch(ctx)
	for _, evt := range resp.AccountData.Events {
		if evt.Type.Type == event.AccountDataDirectChats.Type {
			b.applyDirect(evt)
		}
	}
	for _, rid := range slices.Sorted(maps.Keys(resp.Rooms.Join)) {
		if err := b.applyJoined(rid, resp.Rooms.Join[rid]); err != nil {
			return Changes{}, fmt.Errorf("rooms: %s: %w", rid, err)
		}
	}
	for _, rid := range slices.Sorted(maps.Keys(resp.Rooms.Invite)) {
		if err := b.applyInvite(rid, resp.Rooms.Invite[rid]); err != nil {
			return Changes{}, fmt.Errorf("rooms: invite %s: %w", rid, err)
		}
	}
	for _, rid := range slices.Sorted(maps.Keys(resp.Rooms.Leave)) {
		if err := b.applyLeft(rid, resp.Rooms.Leave[rid]); err != nil {
			return Changes{}, fmt.Errorf("rooms: left %s: %w", rid, err)
		}
	}
	return b.finish()
}

func (b *batch) applyDirect(evt *event.Event) {
	var content map[id.UserID][]id.RoomID
	parseContent(evt, &content)
	direct := map[id.RoomID][]id.UserID{}
	for u, rids := range content {
		for _, rid := range rids {
			if !slices.Contains(direct[rid], u) {
				direct[rid] = append(direct[rid], u)
			}
		}
	}
	for _, us := range direct {
		slices.Sort(us)
	}
	b.direct = direct
}

func (b *batch) applyJoined(rid id.RoomID, jr *mautrix.SyncJoinedRoom) error {
	r, err := b.room(rid)
	if err != nil {
		return err
	}
	r.row.JoinState = store.Joined
	r.inviteDirect = false
	for _, evt := range jr.State.Events {
		if err := b.applyState(r, evt); err != nil {
			return err
		}
	}
	if s := jr.Summary; len(s.Heroes) > 0 || s.JoinedMemberCount != nil || s.InvitedMemberCount != nil {
		if len(s.Heroes) > 0 {
			r.heroes = slices.Clone(s.Heroes)
		}
		if s.JoinedMemberCount != nil {
			r.joinedN, r.haveCounts = *s.JoinedMemberCount, true
		}
		if s.InvitedMemberCount != nil {
			r.invitedN, r.haveCounts = *s.InvitedMemberCount, true
		}
	}
	if err := b.applyTimeline(r, jr.Timeline); err != nil {
		return err
	}
	for _, evt := range jr.Ephemeral.Events {
		var err error
		switch evt.Type.Type {
		case event.EphemeralEventReceipt.Type:
			err = b.applyReceipts(r, evt)
		case event.EphemeralEventTyping.Type:
			b.applyTyping(r, evt)
		}
		if err != nil {
			return err
		}
	}
	for _, evt := range jr.AccountData.Events {
		if evt.Type.Type == markedUnread || evt.Type.Type == markedUnreadV0 {
			var c struct {
				Unread bool `json:"unread"`
			}
			parseContent(evt, &c)
			r.row.MarkedUnread = c.Unread
		}
	}
	if n := jr.UnreadNotifications; n != nil {
		r.row.HighlightCount, r.row.NotificationCount = n.HighlightCount, n.NotificationCount
	}
	return nil
}

func (b *batch) applyInvite(rid id.RoomID, ir *mautrix.SyncInvitedRoom) error {
	r, err := b.room(rid)
	if err != nil {
		return err
	}
	r.row.JoinState = store.Invited
	for _, evt := range ir.State.Events {
		if err := b.applyState(r, evt); err != nil {
			return err
		}
	}
	return nil
}

func (b *batch) applyLeft(rid id.RoomID, lr *mautrix.SyncLeftRoom) error {
	r, err := b.room(rid)
	if err != nil {
		return err
	}
	for _, evt := range lr.State.Events {
		if err := b.applyState(r, evt); err != nil {
			return err
		}
	}
	if err := b.applyTimeline(r, lr.Timeline); err != nil {
		return err
	}
	// After the timeline: its own leave event must not be undone by an
	// earlier state event.
	r.row.JoinState = store.Left
	return nil
}

// applyState applies a state event (from a state section, a timeline, or
// an invite's stripped state) to r.
func (b *batch) applyState(r *room, evt *event.Event) error {
	if evt.StateKey == nil {
		return nil
	}
	key := *evt.StateKey
	switch evt.Type.Type {
	case event.StateRoomName.Type:
		var c struct {
			Name string `json:"name"`
		}
		parseContent(evt, &c)
		r.explicitName = &c.Name
	case event.StateTopic.Type:
		var c struct {
			Topic string `json:"topic"`
		}
		parseContent(evt, &c)
		r.row.Topic = c.Topic
	case event.StateCanonicalAlias.Type:
		var c struct {
			Alias      id.RoomAlias   `json:"alias"`
			AltAliases []id.RoomAlias `json:"alt_aliases"`
		}
		parseContent(evt, &c)
		r.row.CanonicalAlias, r.row.AltAliases = c.Alias, slices.Clone(c.AltAliases)
	case event.StateRoomAvatar.Type:
		var c struct {
			URL id.ContentURIString `json:"url"`
		}
		parseContent(evt, &c)
		r.row.AvatarMXC = c.URL
	case event.StateEncryption.Type:
		var c struct {
			Algorithm string `json:"algorithm"`
		}
		parseContent(evt, &c)
		if c.Algorithm != "" {
			r.row.Encrypted = true
		}
	case event.StateJoinRules.Type:
		var c event.JoinRulesEventContent
		parseContent(evt, &c)
		r.joinRule = c.JoinRule
	case event.StateCreate.Type:
		var c struct {
			Type string `json:"type"`
		}
		parseContent(evt, &c)
		r.row.IsSpace = c.Type == "m.space"
	case event.StateMember.Type:
		return b.applyMember(r, id.UserID(key), evt)
	case event.StateSpaceChild.Type:
		var c event.SpaceChildEventContent
		parseContent(evt, &c)
		return b.setEdge(id.RoomID(key), r.row.ID, c.Order, len(c.Via) > 0, false)
	case event.StateSpaceParent.Type:
		var c event.SpaceParentEventContent
		parseContent(evt, &c)
		return b.setEdge(r.row.ID, id.RoomID(key), "", len(c.Via) > 0, true)
	}
	return nil
}

func (b *batch) applyMember(r *room, user id.UserID, evt *event.Event) error {
	if user == "" {
		return nil
	}
	var c event.MemberEventContent
	parseContent(evt, &c)
	if _, err := b.m.userAlias(b.ctx, user); err != nil {
		return err
	}
	if user == b.m.self && c.Membership == event.MembershipInvite {
		r.inviteDirect, r.inviter = c.IsDirect, evt.Sender
	}
	old, had := r.members[user]
	cur := member{name: c.Displayname, membership: c.Membership}
	if had && old == cur {
		return nil
	}
	r.members[user] = cur
	b.members = true
	if old.name != cur.name {
		b.names[user] = true
	}
	if r.row.JoinState == store.Invited {
		// Stripped invite state is not the room's state; the state
		// store keeps it out too.
		return nil
	}
	return b.m.ss.SetMember(b.ctx, r.row.ID, user, &c)
}

// setEdge records (present) or removes the edge putting child in space.
// An edge from the child's m.space.parent (fromParent) never overrides
// the order of one from the space's m.space.child, and its removal only
// removes an edge without an order.
func (b *batch) setEdge(child, space id.RoomID, order string, present, fromParent bool) error {
	if child == "" || space == "" {
		return nil
	}
	cur, ok := b.currentEdges()[child][space]
	switch {
	case present && fromParent && ok:
		return nil
	case present && ok && cur == order:
		return nil
	case !present && (!ok || (fromParent && cur != "")):
		return nil
	}
	edges := b.edgesForWrite()
	if present {
		if edges[child] == nil {
			edges[child] = map[id.RoomID]string{}
		}
		edges[child][space] = order
		return b.m.st.PutSpaceParent(b.ctx, store.SpaceParent{RoomID: child, SpaceID: space, Order: order})
	}
	delete(edges[child], space)
	if len(edges[child]) == 0 {
		delete(edges, child)
	}
	return b.m.st.DeleteSpaceParent(b.ctx, child, space)
}

// liveTS mints the TS of a live event, its origin capped at maxFuture
// past now.
func (b *batch) liveTS(rid id.RoomID, evt *event.Event) (string, error) {
	origin := time.UnixMilli(evt.Timestamp)
	if limit := b.m.now().Add(maxFuture); origin.After(limit) {
		origin = limit
	}
	return b.m.ids.TSForLive(b.ctx, rid, evt.ID, origin)
}

func (b *batch) applyTimeline(r *room, tl mautrix.SyncTimeline) error {
	rid := r.row.ID
	if tl.Limited && tl.PrevBatch != "" {
		for _, evt := range tl.Events {
			if evt.ID == "" {
				continue
			}
			ts, err := b.liveTS(rid, evt)
			if err != nil {
				return err
			}
			if _, err := b.m.st.AddGap(b.ctx, rid, tl.PrevBatch, ts); err != nil {
				return err
			}
			break
		}
	}
	for _, evt := range tl.Events {
		if evt.StateKey != nil {
			if err := b.applyState(r, evt); err != nil {
				return err
			}
			continue
		}
		if evt.ID == "" {
			continue
		}
		switch evt.Type.Type {
		case typeRedaction:
			if err := b.applyRedaction(r, evt); err != nil {
				return err
			}
		case typeMessage, typeSticker, typeEncrypted, typeReaction:
			ts, err := b.liveTS(rid, evt)
			if err != nil {
				return err
			}
			if err := b.storeEvent(r, evt, ts, true); err != nil {
				return err
			}
		}
	}
	return nil
}

// storeEvent stores a message, sticker, reaction or encrypted event at ts
// and records what it changed.
func (b *batch) storeEvent(r *room, evt *event.Event, ts string, live bool) error {
	rid, st := r.row.ID, b.m.st
	if _, err := b.m.userAlias(b.ctx, evt.Sender); err != nil {
		return err
	}
	raw := rawContent(evt)
	var c msgContent
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &c)
	}
	e := store.Event{
		RoomID: rid, ID: evt.ID, TS: ts, Sender: evt.Sender, Type: evt.Type.Type, Content: raw,
		Decrypted: evt.Mautrix.WasEncrypted, Redacted: evt.Unsigned.RedactedBecause != nil,
		OriginServerTS: evt.Timestamp,
	}
	if c.RelatesTo != nil {
		e.RelatesTo, e.RelType = c.RelatesTo.EventID, c.RelatesTo.RelType
		if e.RelType == relThread {
			e.ThreadRoot = e.RelatesTo
		}
	}
	if (e.Type == typeMessage && e.RelType != relReplace) || e.Type == typeSticker {
		e.Body = c.text()
	}
	prev, existed, err := st.GetEvent(b.ctx, rid, evt.ID)
	if err != nil {
		return err
	}
	if existed && prev.Redacted {
		// A redaction is final; a re-delivery must not bring it back.
		e.Redacted = true
	}
	if e.Redacted {
		e.Content, e.Body = json.RawMessage("{}"), ""
	}
	if existed && sameEvent(prev, e) {
		return nil
	}
	if err := st.PutEvent(b.ctx, e); err != nil {
		return err
	}
	b.timeline[rid] = true
	if ts > r.row.LastEventTS {
		r.row.LastEventTS = ts
	}
	// first is true the first time the event's real type is known: it
	// is new, or it was stored encrypted and is decrypted now.
	first := !existed || (prev.Type == typeEncrypted && e.Type != typeEncrypted)
	change := TimelineChange{Room: rid, Event: evt.ID, TS: ts, Thread: e.ThreadRoot, Live: live}
	switch {
	case e.Type == typeMessage && e.RelType == relReplace:
		target := e.RelatesTo
		latest, orig, ok, err := b.refreshBody(rid, target)
		if err != nil || !ok || latest != evt.ID || e.Redacted {
			return err
		}
		b.ch.Timeline = append(b.ch.Timeline, TimelineChange{
			Kind: Edited, Room: rid, Event: target, TS: orig.TS, Thread: orig.ThreadRoot, Live: live,
		})
	case e.Type == typeReaction:
		if !first || e.Redacted || e.RelType != relAnnotation {
			return nil
		}
		return b.reactionChange(ReactionAdded, rid, e.RelatesTo, c.RelatesTo.Key, e.Sender, live)
	default:
		if _, _, _, err := b.refreshBody(rid, evt.ID); err != nil {
			return err
		}
		switch {
		case !existed && !e.Redacted:
			change.Kind = Added
		case existed && !prev.Redacted && e.Redacted:
			change.Kind = Redacted
		case existed && first && !e.Redacted:
			change.Kind = Edited
		default:
			return nil
		}
		b.ch.Timeline = append(b.ch.Timeline, change)
	}
	return nil
}

// sameEvent reports whether a and b are the same stored event, ignoring
// the body (an edit may have replaced it).
func sameEvent(a, b store.Event) bool {
	if !bytes.Equal(a.Content, b.Content) {
		return false
	}
	a.Content, b.Content, a.Body, b.Body = nil, nil, "", ""
	return reflect.DeepEqual(a, b)
}

// reactionChange records a reaction to target, if target has a TS.
func (b *batch) reactionChange(kind ChangeKind, rid id.RoomID, target id.EventID, key string, sender id.UserID, live bool) error {
	ts, ok, err := b.m.ids.TS(b.ctx, rid, target)
	if err != nil || !ok {
		return err
	}
	var thread id.EventID
	if t, ok, err := b.m.st.GetEvent(b.ctx, rid, target); err != nil {
		return err
	} else if ok {
		thread = t.ThreadRoot
	}
	b.ch.Timeline = append(b.ch.Timeline, TimelineChange{
		Kind: kind, Room: rid, Event: target, TS: ts, Thread: thread, Key: key, Sender: sender, Live: live,
	})
	return nil
}

// refreshBody sets the body of message target to the text of its latest
// valid edit (an unredacted m.replace from the same sender), or its own
// text when it has none. It returns the latest edit ("" for none) and the
// stored message; ok is false when target is not a stored, unredacted
// message.
func (b *batch) refreshBody(rid id.RoomID, target id.EventID) (latest id.EventID, orig store.Event, ok bool, err error) {
	st := b.m.st
	orig, found, err := st.GetEvent(b.ctx, rid, target)
	if err != nil || !found || orig.Redacted || orig.RelType == relReplace ||
		(orig.Type != typeMessage && orig.Type != typeSticker && orig.Type != typeEncrypted) {
		return "", orig, false, err
	}
	var oc msgContent
	_ = json.Unmarshal(orig.Content, &oc)
	body := ""
	if orig.Type != typeEncrypted {
		body = oc.text()
	}
	edits, err := st.Relations(b.ctx, rid, target, relReplace)
	if err != nil {
		return "", orig, false, err
	}
	for i := len(edits) - 1; i >= 0; i-- {
		ed := edits[i]
		if ed.Redacted || ed.Sender != orig.Sender || ed.Type != typeMessage {
			continue
		}
		latest = ed.ID
		var ec msgContent
		_ = json.Unmarshal(ed.Content, &ec)
		if ec.NewContent != nil {
			body = ec.NewContent.Body
		}
		break
	}
	if body != orig.Body {
		orig.Body = body
		if err := st.PutEvent(b.ctx, orig); err != nil {
			return "", orig, false, err
		}
		b.timeline[rid] = true
	}
	return latest, orig, true, nil
}

func (b *batch) applyRedaction(r *room, evt *event.Event) error {
	rid, st := r.row.ID, b.m.st
	target := evt.Redacts
	if target == "" {
		var c msgContent
		parseContent(evt, &c)
		target = c.Redacts
	}
	if target == "" {
		return nil
	}
	t, ok, err := st.GetEvent(b.ctx, rid, target)
	if err != nil || !ok || t.Redacted {
		return err
	}
	if err := st.RedactEvent(b.ctx, rid, target); err != nil {
		return err
	}
	b.timeline[rid] = true
	switch {
	case t.Type == typeReaction:
		var c msgContent
		_ = json.Unmarshal(t.Content, &c)
		if t.RelType != relAnnotation || c.RelatesTo == nil {
			return nil
		}
		return b.reactionChange(ReactionRemoved, rid, t.RelatesTo, c.RelatesTo.Key, t.Sender, true)
	case t.RelType == relReplace:
		_, orig, ok, err := b.refreshBody(rid, t.RelatesTo)
		if err != nil || !ok {
			return err
		}
		b.ch.Timeline = append(b.ch.Timeline, TimelineChange{
			Kind: Edited, Room: rid, Event: orig.ID, TS: orig.TS, Thread: orig.ThreadRoot, Live: true,
		})
	default:
		b.ch.Timeline = append(b.ch.Timeline, TimelineChange{
			Kind: Redacted, Room: rid, Event: target, TS: t.TS, Thread: t.ThreadRoot, Live: true,
		})
	}
	return nil
}

func (b *batch) applyReceipts(r *room, evt *event.Event) error {
	var content event.ReceiptEventContent
	parseContent(evt, &content)
	rid := r.row.ID
	for _, ev := range slices.Sorted(maps.Keys(content)) {
		for _, rtype := range slices.Sorted(maps.Keys(content[ev])) {
			if rtype != event.ReceiptTypeRead && rtype != event.ReceiptTypeReadPrivate {
				continue
			}
			users := content[ev][rtype]
			for _, user := range slices.Sorted(maps.Keys(users)) {
				thread := string(users[user].ThreadID)
				ts, _, err := b.m.ids.TS(b.ctx, rid, ev)
				if err != nil {
					return err
				}
				var readAt int64
				if at := users[user].Timestamp; !at.IsZero() {
					readAt = at.UnixMilli()
				}
				if user == b.m.self {
					if err := b.ownReceipt(r, thread, ev, ts, readAt); err != nil {
						return err
					}
					continue
				}
				rec := store.Receipt{RoomID: rid, ThreadID: thread, UserID: user, EventID: ev, TS: ts, ReadAt: readAt}
				moved, err := b.m.st.AdvanceReceipt(b.ctx, rec)
				if err != nil {
					return err
				}
				if moved {
					b.ch.Receipts = append(b.ch.Receipts, UserReceipt{
						Room: rid, User: user, Thread: thread, Event: ev, TS: ts,
					})
				}
			}
		}
	}
	return nil
}

// ownReceipt moves the account's receipt in r's thread to ev, unless it
// is already there or later. A receipt on an event without a TS (older
// than anything stored) only replaces one that has no TS either.
func (b *batch) ownReceipt(r *room, thread string, ev id.EventID, ts string, readAt int64) error {
	cur, ok := r.receipts[thread]
	if ok && (cur.EventID == ev || (ts == "" && cur.TS != "") || (ts != "" && cur.TS != "" && ts <= cur.TS)) {
		return nil
	}
	rec := store.Receipt{RoomID: r.row.ID, ThreadID: thread, UserID: b.m.self, EventID: ev, TS: ts, ReadAt: readAt}
	if err := b.m.st.PutReceipt(b.ctx, rec); err != nil {
		return err
	}
	r.receipts[thread] = rec
	b.ch.OwnReceipts = append(b.ch.OwnReceipts, receiptOf(rec))
	return nil
}

// receiptOf is the contract's Receipt for a stored own receipt.
func receiptOf(rec store.Receipt) Receipt {
	thread := id.EventID(rec.ThreadID)
	if thread == event.ReadReceiptThreadMain {
		thread = ""
	}
	return Receipt{Room: rec.RoomID, Thread: thread, Event: rec.EventID, TS: rec.TS}
}

func (b *batch) applyTyping(r *room, evt *event.Event) {
	var c struct {
		UserIDs []id.UserID `json:"user_ids"`
	}
	parseContent(evt, &c)
	users := []id.UserID{}
	for _, u := range c.UserIDs {
		if u != b.m.self {
			users = append(users, u)
		}
	}
	slices.Sort(users)
	if b.ch.Typing == nil {
		b.ch.Typing = map[id.RoomID][]id.UserID{}
	}
	b.ch.Typing[r.row.ID] = users
}

// finish computes what the batch's inputs imply (m.direct, names, kinds,
// unread state), persists the rows that changed, works out Changes and
// publishes the batch.
func (b *batch) finish() (Changes, error) {
	m := b.m
	m.mu.RLock()
	published := maps.Clone(m.rooms)
	oldEdges := m.edges
	direct := m.direct
	oldGlobal := m.global
	m.mu.RUnlock()

	// m.direct: a new one applies to every room, a known one to new rooms.
	if b.direct != nil {
		direct = b.direct
		for rid, pub := range published {
			if _, ok := b.rooms[rid]; ok {
				continue
			}
			if us := direct[rid]; pub.direct != (len(us) > 0) || !slices.Equal(pub.directUsers, us) || pub.minOthers != 0 {
				if _, err := b.room(rid); err != nil {
					return Changes{}, err
				}
			}
		}
		for _, r := range b.rooms {
			us := direct[r.row.ID]
			r.direct, r.directUsers, r.minOthers = len(us) > 0, slices.Clone(us), 0
		}
	}

	all := func() map[id.RoomID]*room {
		out := maps.Clone(published)
		maps.Copy(out, b.rooms)
		return out
	}
	global := oldGlobal
	if b.members {
		global = buildGlobal(all())
	}
	dn := func(rid id.RoomID, user id.UserID) string {
		r := b.rooms[rid]
		if r == nil {
			r = published[rid]
		}
		return displayName(r, global, user)
	}

	// Names and kinds: the touched rooms, and every room when display
	// names may have moved.
	derive := func(r *room) (store.RoomKind, id.UserID, string) {
		kind, dmUser := r.kind(m.self)
		return kind, dmUser, r.name(m.self, kind, dmUser, dn)
	}
	for rid, pub := range published {
		if _, ok := b.rooms[rid]; ok || !b.members {
			continue
		}
		if kind, dmUser, name := derive(pub); kind != pub.row.Kind || dmUser != pub.row.DMUserID || name != pub.row.Name {
			if _, err := b.room(rid); err != nil {
				return Changes{}, err
			}
		}
	}
	for _, r := range b.rooms {
		r.row.Kind, r.row.DMUserID, r.row.Name = derive(r)
		if r.row.DMUserID != "" {
			if _, err := m.userAlias(b.ctx, r.row.DMUserID); err != nil {
				return Changes{}, err
			}
		}
	}

	// Unread state of rooms whose events changed, and own receipts that
	// were on events not stored before.
	for rid := range b.timeline {
		r := b.rooms[rid]
		var err error
		if r.candTS, err = m.st.LatestUnreadCandidate(b.ctx, rid, m.self); err != nil {
			return Changes{}, err
		}
		if r.ownTS, err = m.st.LatestSentBy(b.ctx, rid, m.self); err != nil {
			return Changes{}, err
		}
		for _, thread := range slices.Sorted(maps.Keys(r.receipts)) {
			rec := r.receipts[thread]
			if rec.TS != "" {
				continue
			}
			ts, ok, err := m.ids.TS(b.ctx, rid, rec.EventID)
			if err != nil {
				return Changes{}, err
			}
			if !ok {
				continue
			}
			rec.TS = ts
			if err := m.st.PutReceipt(b.ctx, rec); err != nil {
				return Changes{}, err
			}
			r.receipts[thread] = rec
			b.ch.OwnReceipts = append(b.ch.OwnReceipts, receiptOf(rec))
		}
	}

	// Persist changed rows.
	for _, rid := range slices.Sorted(maps.Keys(b.rooms)) {
		r := b.rooms[rid]
		if r.row.JoinState == "" {
			// Only seen through an edge or a decrypted event; not a
			// room the account has joined, been invited to or left.
			r.row.JoinState = store.Left
		}
		r.saveInputs()
		if pub := published[rid]; pub != nil && rowEqual(pub.row, r.row) {
			continue
		}
		if err := m.st.PutRoom(b.ctx, r.row); err != nil {
			return Changes{}, err
		}
	}

	// Changes.
	newAll := all()
	newEdges := oldEdges
	if b.edges != nil {
		newEdges = b.edges
	}
	getOld := func(rid id.RoomID) *room { return published[rid] }
	getNew := func(rid id.RoomID) *room { return newAll[rid] }
	for _, rid := range slices.Sorted(maps.Keys(newAll)) {
		nr, or := newAll[rid], published[rid]
		nv := nr.view(sidebarSection(nr, getNew, newEdges))
		if or == nil || or.view(sidebarSection(or, getOld, oldEdges)) != nv {
			b.ch.Rooms = append(b.ch.Rooms, rid)
		}
	}
	for _, rid := range slices.Sorted(maps.Keys(b.rooms)) {
		r, was := b.rooms[rid], b.wasJoined[rid]
		switch {
		case r.row.JoinState == store.Joined && was != store.Joined:
			b.ch.Joined = append(b.ch.Joined, rid)
		case r.row.JoinState == store.Left && (was == store.Joined || was == store.Invited):
			b.ch.Left = append(b.ch.Left, rid)
		}
		if pub := published[rid]; pub == nil || pub.readState() != r.readState() {
			b.ch.ReadStates = append(b.ch.ReadStates, rid)
		}
	}
	b.ch.Names = slices.Sorted(maps.Keys(b.names))

	// Publish.
	m.mu.Lock()
	for rid, r := range b.rooms {
		if _, ok := m.rooms[rid]; !ok {
			byAlias := maps.Clone(m.byAlias)
			byAlias[r.alias] = rid
			m.byAlias = byAlias
		}
		m.rooms[rid] = r
	}
	m.edges = newEdges
	m.direct = direct
	m.global = global
	m.mu.Unlock()
	return b.ch, nil
}

// sidebarSection is r's section ID, "" for a room that is not a sidebar
// row.
func sidebarSection(r *room, get func(id.RoomID) *room, edges map[id.RoomID]map[id.RoomID]string) string {
	if !r.sidebar() {
		return ""
	}
	return sectionOf(r, get, edges)
}

// rowEqual compares two rows, ignoring UpdatedAt.
func rowEqual(a, b store.Room) bool {
	a.UpdatedAt, b.UpdatedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// buildGlobal returns every known user's global display name: the
// non-empty name of their member event in the first room (by ID) that has
// one.
func buildGlobal(rooms map[id.RoomID]*room) map[id.UserID]string {
	out := map[id.UserID]string{}
	for _, rid := range slices.Sorted(maps.Keys(rooms)) {
		for u, mem := range rooms[rid].members {
			if _, ok := out[u]; !ok && mem.name != "" {
				out[u] = mem.name
			}
		}
	}
	return out
}

// displayName is user's name in r (nil for none): the member name there,
// else the global name, else the localpart.
func displayName(r *room, global map[id.UserID]string, user id.UserID) string {
	if r != nil {
		if mem, ok := r.members[user]; ok && mem.name != "" {
			return mem.name
		}
	}
	if n := global[user]; n != "" {
		return n
	}
	return localpart(user)
}
