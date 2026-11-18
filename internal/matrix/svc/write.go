package svc

// WP-Write: send, edit, delete, reactions, receipts, joins, typing. This file is the work package's; it owns writeState
// (the Backend's write field) and fillWrite.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/emoji"
	"github.com/gammons/slk/internal/ids"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/session/e2ee"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// The write side: what the user does to the account, as requests to the
// homeserver. Nothing here writes the store or the room model: what is
// sent comes back through /sync (svc.go), and the live side shows it.
//
// Sent messages and the UI. A send returns its event ID; TSForSent gives
// it its TS then, so the /sync echo (TSForLive) maps to the same TS, or,
// when the echo was applied first, TSForSent returns the echo's. The UI
// swaps its "local:<n>" placeholder for the MessageSentMsg's message and
// records that TS (selfSend.RecordSent), and drops the echo by it
// (IsSelfSent), or, when the echo comes first, while the send is in flight.
//
// Port by port:
//
//	Send            mxfmt.FromCompose, m.room.message
//	Edit            m.replace with m.new_content (event.SetEdit)
//	Delete          redaction
//	Permalink       matrix.to link to the event, via the account's server
//	MarkUnread      a room: m.marked_unread room account data, since
//	                receipts never move back, so the room shows unread but
//	                its read position stays; a thread: local only (Matrix
//	                has no unread mark for a thread)
//	SendReply       m.thread relation with the newest reply (or the root)
//	                as reply fallback; broadcast marks the reply and also
//	                sends a marked plain room copy (see sendReply)
//	thread Mark     m.read.private receipt for the thread (thread_id)
//	MarkRead        m.read.private receipt for the main timeline (thread_id
//	                "main") and m.fully_read; clears m.marked_unread
//	Join            join; accepting a direct invite adds the room to m.direct
//	OpenConversation the existing DM with the one user, else a new
//	                encrypted room (trusted_private_chat, is_direct) added to
//	                m.direct; answers once /sync has brought the room
//	MembershipFetch loads the room's full member list (/members) and tells
//	                the UI through SetUISend's sender (setSend)
//	reactions       shortcode -> glyph key (internal/emoji); removing
//	                redacts the account's reaction with that key
//
// Encrypted rooms: the sync filter lazy-loads members, so mautrix's state
// store, which the crypto machine shares a new Megolm session from, may
// miss members who never spoke. ensureMembers loads the full list before
// the first encrypted send in a room.

const (
	// openWait is how often OpenConversation looks for the new room in
	// the model while it waits for /sync to bring it.
	openWait = 100 * time.Millisecond
	// markedUnreadType is MSC2867's room account data (rooms reads it,
	// and its unstable name, the same way).
	markedUnreadType = "m.marked_unread"
)

// writeState is WP-Write's state on a Backend.
type writeState struct {
	mu sync.Mutex
	// send delivers a message to the UI (the live side's, setSend);
	// nil until set.
	send func(tea.Msg)
	// members are the rooms whose full member list is in the state
	// store; memberMu serializes the loads.
	members  map[id.RoomID]bool
	memberMu sync.Mutex
	// directMu serializes m.direct's read-modify-write.
	directMu sync.Mutex
	// reactions are the account's reactions sent by this process, by
	// what they react to and their key, so Remove finds one whose echo
	// has not been stored yet.
	reactions map[reactionRef]id.EventID
}

// reactionRef names a reaction: its room, target and key.
type reactionRef struct {
	room   id.RoomID
	target id.EventID
	key    string
}

func newWriteState() *writeState {
	return &writeState{members: map[id.RoomID]bool{}, reactions: map[reactionRef]id.EventID{}}
}

// setSend sets how MembershipFetch tells the UI the members it loaded
// (SetUISend).
func (w *writeState) setSend(send func(tea.Msg)) {
	w.mu.Lock()
	w.send = send
	w.mu.Unlock()
}

func (w *writeState) sender() func(tea.Msg) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.send
}

// fillWrite sets WP-Write's ports on p.
func (b *Backend) fillWrite(p *ports) {
	p.messages.Send = func(ch ids.ChannelID, text string) core.Msg { return b.send(string(ch), text) }
	p.messages.Edit = func(ch ids.ChannelID, ts ids.MessageTS, text string) core.Msg {
		return b.edit(string(ch), string(ts), text)
	}
	p.messages.Delete = func(ch ids.ChannelID, ts ids.MessageTS) core.Msg { return b.delete(string(ch), string(ts)) }
	p.messages.MarkUnread = func(ch ids.ChannelID, thread ids.ThreadTS, boundary ids.MessageTS, n int) core.Msg {
		return b.markUnread(string(ch), string(thread), string(boundary), n)
	}
	p.messages.Permalink = func(ctx context.Context, ch ids.ChannelID, ts ids.MessageTS, _ ids.ThreadTS) (string, error) {
		return b.permalink(ctx, string(ch), string(ts))
	}

	p.threads.SendReply = func(ch ids.ChannelID, thread ids.ThreadTS, text string, broadcast bool) core.Msg {
		return b.sendReply(string(ch), string(thread), text, broadcast)
	}
	p.threads.Mark = func(ch ids.ChannelID, thread ids.ThreadTS, ts ids.MessageTS) core.Cmd {
		return func() core.Msg { return b.markThread(string(ch), string(thread), string(ts)) }
	}

	p.channels.MarkRead = func(ch ids.ChannelID, ts ids.MessageTS) core.Msg { return b.markRead(string(ch), string(ts)) }
	p.channels.Join = func(ch ids.ChannelID, _ string) core.Msg { return b.join(string(ch)) }
	p.channels.OpenConversation = func(users []string, req uint64) core.Cmd {
		users = slices.Clone(users)
		return func() core.Msg { return b.openConversation(users, req) }
	}
	p.channels.MembershipFetch = func(ch ids.ChannelID) { b.membershipFetch(string(ch)) }

	p.reactions = core.NewReactionService(
		func(ch ids.ChannelID, ts ids.MessageTS, name string) error {
			return b.react(string(ch), string(ts), name)
		},
		func(ch ids.ChannelID, ts ids.MessageTS, name string) error {
			return b.unreact(string(ch), string(ts), name)
		},
		nil, nil,
	)
}

// errNoClient is a write on a store-only backend.
var errNoClient = errors.New("svc: not connected")

// writeCtx resolves a channel alias (a room the model knows, else one
// ident has an alias for) and starts a write's context.
func (b *Backend) writeCtx(alias string) (context.Context, context.CancelFunc, id.RoomID, error) {
	if b.client == nil {
		return nil, nil, "", errNoClient
	}
	room, ok := b.rooms.Resolver().RoomID(alias)
	if !ok {
		return nil, nil, "", errUnknownRoom
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	return ctx, cancel, room, nil
}

// eventAtTS returns the event ts names in room.
func (b *Backend) eventAtTS(ctx context.Context, room id.RoomID, ts string) (id.EventID, error) {
	ev, ok, err := b.d.IDs.Event(ctx, room, ts)
	if err == nil && !ok {
		err = errors.New("svc: no message with that TS")
	}
	return ev, err
}

// sendMessage sends content to room, after loading the room's members
// when it is encrypted, and returns the event and its TS.
func (b *Backend) sendMessage(ctx context.Context, room id.RoomID, content *event.MessageEventContent) (id.EventID, string, error) {
	return b.sendContent(ctx, room, content, nil)
}

// sendContent is sendMessage with extra top-level content keys (nil for
// none).
func (b *Backend) sendContent(ctx context.Context, room id.RoomID, content *event.MessageEventContent, extra map[string]any) (id.EventID, string, error) {
	if err := b.ensureMembers(ctx, room); err != nil {
		return "", "", err
	}
	var payload any = content
	if extra != nil {
		payload = &event.Content{Parsed: content, Raw: extra}
	}
	resp, err := b.client.SendMessageEvent(ctx, room, event.EventMessage, payload)
	if err != nil {
		return "", "", err
	}
	ts, err := b.d.IDs.TSForSent(ctx, room, resp.EventID, time.Now())
	if err != nil {
		// Sent all the same; the echo shows it.
		debuglog.General("matrix send: TS of %s in %s: %v", resp.EventID, room, err)
	}
	return resp.EventID, ts, nil
}

// sentItem is a message this client sent as the UI shows it, as the live
// side will show its echo.
func (b *Backend) sentItem(room id.RoomID, ts string, content *event.MessageEventContent) core.MessageItem {
	res := b.rooms.Resolver()
	it := core.MessageItem{
		TS:        ts,
		UserID:    res.UserAlias(b.self),
		UserName:  b.rooms.DisplayName(room, b.self),
		Timestamp: b.formatTS(ts),
		Text:      messageText(content, res),
	}
	if content.MsgType == event.MsgEmote {
		it.Subtype = "me_message"
	}
	return it
}

// send is MessageService.Send.
func (b *Backend) send(alias, text string) core.Msg {
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return ui.MessageSendFailedMsg{ChannelID: alias, Reason: err.Error()}
	}
	defer cancel()
	content := mxfmt.FromCompose(text, b.rooms.Resolver())
	_, ts, err := b.sendMessage(ctx, room, content)
	if err != nil {
		return ui.MessageSendFailedMsg{ChannelID: alias, Reason: err.Error()}
	}
	if ts == "" {
		return ui.MessageSentMsg{ChannelID: alias}
	}
	return ui.MessageSentMsg{ChannelID: alias, Message: b.sentItem(room, ts, content)}
}

// sendReply is ThreadService.SendReply. The reply's reply fallback (for
// clients without threads) is the thread's newest stored reply, else the
// root. A broadcast reply is marked broadcastKey and also sent to the
// room as a plain message marked broadcastCopyKey, for other clients; slk
// shows the reply in the room's feed (items.go) and hides the copy, so it
// is one message in both places, as Slack's thread_broadcast is.
func (b *Backend) sendReply(alias, threadTS, text string, broadcast bool) core.Msg {
	failed := func(err error) core.Msg {
		return ui.ThreadReplySendFailedMsg{ChannelID: alias, ThreadTS: threadTS, Reason: err.Error(), Broadcast: broadcast}
	}
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return failed(err)
	}
	defer cancel()
	root, err := b.eventAtTS(ctx, room, threadTS)
	if err != nil {
		return failed(err)
	}
	fallback := root
	if rows, err := b.threadRows(ctx, room, root); err == nil {
		for _, r := range slices.Backward(rows) {
			if r.ID != root {
				fallback = r.ID
				break
			}
		}
	}
	res := b.rooms.Resolver()
	content := mxfmt.FromCompose(text, res)
	plain := *content
	content.RelatesTo = (&event.RelatesTo{}).SetThread(root, fallback)
	var extra map[string]any
	if broadcast {
		extra = map[string]any{broadcastKey: true}
	}
	reply, ts, err := b.sendContent(ctx, room, content, extra)
	if err != nil {
		return failed(err)
	}
	if broadcast {
		if _, _, err := b.sendContent(ctx, room, &plain, map[string]any{broadcastCopyKey: reply}); err != nil {
			debuglog.General("matrix reply broadcast in %s: %v", room, err)
		}
	}
	out := ui.ThreadReplySentMsg{ChannelID: alias, ThreadTS: threadTS, Broadcast: broadcast}
	if ts != "" {
		out.Message = b.sentItem(room, ts, &plain)
		out.Message.ThreadTS = threadTS
		if broadcast {
			out.Message.Subtype = "thread_broadcast"
		}
	}
	return out
}

// edit is MessageService.Edit.
func (b *Backend) edit(alias, ts, text string) core.Msg {
	out := ui.MessageEditedMsg{ChannelID: alias, TS: ts}
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		out.Err = err
		return out
	}
	defer cancel()
	orig, err := b.eventAtTS(ctx, room, ts)
	if err != nil {
		out.Err = err
		return out
	}
	content := mxfmt.FromCompose(text, b.rooms.Resolver())
	content.SetEdit(orig)
	_, _, out.Err = b.sendMessage(ctx, room, content)
	return out
}

// delete is MessageService.Delete: a redaction.
func (b *Backend) delete(alias, ts string) core.Msg {
	out := ui.MessageDeletedMsg{ChannelID: alias, TS: ts}
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		out.Err = err
		return out
	}
	defer cancel()
	ev, err := b.eventAtTS(ctx, room, ts)
	if err == nil {
		_, err = b.client.RedactEvent(ctx, room, ev)
	}
	out.Err = err
	return out
}

// permalink is MessageService.Permalink: a matrix.to link to the event.
// A thread reply's link is its own event's; Matrix clients open its
// thread from it.
func (b *Backend) permalink(ctx context.Context, alias, ts string) (string, error) {
	room, ok := b.rooms.Resolver().RoomID(alias)
	if !ok {
		return "", errUnknownRoom
	}
	ev, err := b.eventAtTS(ctx, room, ts)
	if err != nil {
		return "", err
	}
	return room.EventURI(ev, b.self.Homeserver()).MatrixToURL(), nil
}

// markUnread is MessageService.MarkUnread; see the table above for the
// approximation.
func (b *Backend) markUnread(alias, threadTS, boundary string, n int) core.Msg {
	out := ui.MessageMarkedUnreadMsg{ChannelID: alias, ThreadTS: threadTS, BoundaryTS: boundary, UnreadCount: n}
	if threadTS != "" {
		return out
	}
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		out.Err = err
		return out
	}
	defer cancel()
	out.Err = b.client.SetRoomAccountData(ctx, room, markedUnreadType, map[string]bool{"unread": true})
	return out
}

// markRead is ChannelService.MarkRead. A receipt never moves back: one at
// or before the room's read position is not sent. A room marked unread is
// unmarked.
func (b *Backend) markRead(alias, ts string) core.Msg {
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return nil
	}
	defer cancel()
	if row, ok, err := b.d.Store.GetRoom(ctx, room); err == nil && ok && row.MarkedUnread {
		if err := b.client.SetRoomAccountData(ctx, room, markedUnreadType, map[string]bool{"unread": false}); err != nil {
			debuglog.General("matrix mark read: unmark %s: %v", room, err)
		}
	}
	if ts <= b.rooms.Snapshot().ReadStates[alias].LastReadTS {
		return ui.ChannelMarkedReadMsg{ChannelID: alias}
	}
	ev, err := b.eventAtTS(ctx, room, ts)
	if err != nil {
		debuglog.General("matrix mark read: %s %s: %v", room, ts, err)
		return nil
	}
	err = b.client.SendReceipt(ctx, room, ev, event.ReceiptTypeReadPrivate, &mautrix.ReqSendReceipt{ThreadID: string(event.ReadReceiptThreadMain)})
	if err == nil {
		err = b.client.SetReadMarkers(ctx, room, &mautrix.ReqSetReadMarkers{FullyRead: ev})
	}
	if err != nil {
		debuglog.General("matrix mark read: %s %s: %v", room, ev, err)
		return nil
	}
	return ui.ChannelMarkedReadMsg{ChannelID: alias}
}

// markThread is ThreadService.Mark: a private receipt in the thread, not
// sent when the thread's receipt is already at or after ts.
func (b *Backend) markThread(alias, threadTS, ts string) core.Msg {
	out := ui.ThreadMarkedLocalMsg{ChannelID: alias, ThreadTS: threadTS, TS: ts}
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		out.Err = err
		return out
	}
	defer cancel()
	root, err := b.eventAtTS(ctx, room, threadTS)
	if err != nil {
		out.Err = err
		return out
	}
	if r, ok := b.rooms.ThreadReceipt(room, root); ok && ts <= r.TS {
		return out
	}
	ev, err := b.eventAtTS(ctx, room, ts)
	if err != nil {
		out.Err = err
		return out
	}
	out.Err = b.client.SendReceipt(ctx, room, ev, event.ReceiptTypeReadPrivate, &mautrix.ReqSendReceipt{ThreadID: string(root)})
	return out
}

// join is ChannelService.Join. Joining a direct invite adds the room to
// m.direct under the inviter, as Element does: rooms only shows a joined
// room as a DM when m.direct lists it.
func (b *Backend) join(alias string) core.Msg {
	failed := func(err error) core.Msg { return ui.ChannelJoinFailedMsg{ID: alias, Err: err} }
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return failed(err)
	}
	defer cancel()
	row, stored, err := b.d.Store.GetRoom(ctx, room)
	if err != nil {
		return failed(err)
	}
	if _, err := b.client.JoinRoomByID(ctx, room); err != nil {
		return failed(err)
	}
	if stored && row.JoinState == store.Invited && row.Kind == store.RoomDM && row.DMUserID != "" {
		if err := b.addDirect(ctx, room, []id.UserID{row.DMUserID}); err != nil {
			debuglog.General("matrix join: m.direct for %s: %v", room, err)
		}
	}
	name := row.Name
	if r, ok := b.rooms.RoomByID(room); ok {
		name = r.Name
	}
	return ui.ChannelJoinedMsg{ID: alias, Name: name}
}

// addDirect lists room under each of users in the account's m.direct.
func (b *Backend) addDirect(ctx context.Context, room id.RoomID, users []id.UserID) error {
	w := b.write
	w.directMu.Lock()
	defer w.directMu.Unlock()
	direct := event.DirectChatsEventContent{}
	err := b.client.GetAccountData(ctx, event.AccountDataDirectChats.Type, &direct)
	if err != nil && !errors.Is(err, mautrix.MNotFound) {
		return err
	}
	changed := false
	for _, u := range users {
		if !slices.Contains(direct[u], room) {
			direct[u] = append(direct[u], room)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return b.client.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct)
}

// openConversation is ChannelService.OpenConversation. With one user it
// opens the existing DM with them if the sidebar has one; otherwise (and
// for a group, where it does not look for an existing one) it creates an
// encrypted direct room inviting users and adds it to m.direct. It
// answers once /sync has brought the new room as a DM, or at the timeout,
// so the UI's first Fetch finds the room.
func (b *Backend) openConversation(users []string, req uint64) core.Msg {
	failed := func(err error) core.Msg { return ui.NewMessageFailedMsg{RequestID: req, Err: err} }
	if b.client == nil {
		return failed(errNoClient)
	}
	if len(users) == 0 {
		return failed(errors.New("svc: no users"))
	}
	res := b.rooms.Resolver()
	mxids := make([]id.UserID, 0, len(users))
	for _, u := range users {
		mx, ok := res.MXID(u)
		if !ok {
			return failed(fmt.Errorf("svc: unknown user %s", u))
		}
		mxids = append(mxids, mx)
	}
	opened := ui.NewMessageOpenedMsg{UserIDs: users, RequestID: req}
	if len(users) == 1 {
		for _, c := range b.rooms.Snapshot().Channels {
			if c.Type == string(store.RoomDM) && c.DMUserID == users[0] {
				opened.ChannelID, opened.AlreadyOpen = c.ID, true
				return opened
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()
	resp, err := b.client.CreateRoom(ctx, &mautrix.ReqCreateRoom{
		Preset:   "trusted_private_chat",
		IsDirect: true,
		Invite:   mxids,
		InitialState: []*event.Event{{
			Type:    event.StateEncryption,
			Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}},
		}},
	})
	if err != nil {
		return failed(err)
	}
	if err := b.addDirect(ctx, resp.RoomID, mxids); err != nil {
		return failed(err)
	}
	for ctx.Err() == nil {
		if r, ok := b.rooms.RoomByID(resp.RoomID); ok && (r.Kind == store.RoomDM || r.Kind == store.RoomGroupDM) {
			break
		}
		select {
		case <-ctx.Done():
		case <-time.After(openWait):
		}
	}
	opened.ChannelID = res.RoomAlias(resp.RoomID)
	if opened.ChannelID == "" {
		return failed(errUnknownRoom)
	}
	return opened
}

// membershipFetch is ChannelService.MembershipFetch: the room's full
// member list, loaded once, then told to the UI as ChannelMembershipMsg
// (when setSend has given it a sender).
func (b *Backend) membershipFetch(alias string) {
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return
	}
	defer cancel()
	if err := b.loadMembers(ctx, room); err != nil {
		debuglog.General("matrix members of %s: %v", room, err)
		return
	}
	send := b.write.sender()
	if send == nil || b.client.StateStore == nil {
		return
	}
	members, err := b.client.StateStore.GetRoomJoinedOrInvitedMembers(ctx, room)
	if err != nil {
		return
	}
	res := b.rooms.Resolver()
	out := make([]string, 0, len(members))
	for _, u := range members {
		if a := res.UserAlias(u); a != "" {
			out = append(out, a)
		}
	}
	send(ui.ChannelMembershipMsg{ChannelID: alias, MemberIDs: out})
}

// ensureMembers loads room's full member list before an encrypted send:
// the crypto machine shares a new Megolm session with the members in the
// state store, which lazy-loading sync leaves incomplete. Whether the room
// is encrypted is the state store's answer, as for SendMessageEvent.
func (b *Backend) ensureMembers(ctx context.Context, room id.RoomID) error {
	ss := b.client.StateStore
	if ss == nil {
		return nil
	}
	if enc, err := ss.IsEncrypted(ctx, room); err != nil || !enc {
		return err
	}
	return b.loadMembers(ctx, room)
}

// loadMembers fetches room's members (/members) into the state store
// unless that was done (by this process, or mautrix's members_fetched).
// When it finds members the store did not have, the room's outbound
// Megolm session is dropped, so the next send shares a new one with them
// too (a membership change in /sync does the same).
func (b *Backend) loadMembers(ctx context.Context, room id.RoomID) error {
	ss := b.client.StateStore
	if ss == nil {
		return nil
	}
	w := b.write
	w.memberMu.Lock()
	defer w.memberMu.Unlock()
	if w.members[room] {
		return nil
	}
	if fetched, err := ss.HasFetchedMembers(ctx, room); err != nil {
		return err
	} else if fetched {
		w.members[room] = true
		return nil
	}
	before, err := ss.GetRoomJoinedOrInvitedMembers(ctx, room)
	if err != nil {
		return err
	}
	if _, err := b.client.Members(ctx, room); err != nil {
		return err
	}
	after, err := ss.GetRoomJoinedOrInvitedMembers(ctx, room)
	if err != nil {
		return err
	}
	if slices.ContainsFunc(after, func(u id.UserID) bool { return !slices.Contains(before, u) }) {
		if h, ok := b.client.Crypto.(*e2ee.Helper); ok {
			if err := h.CryptoStore.RemoveOutboundGroupSession(ctx, room); err != nil {
				return err
			}
		}
	}
	w.members[room] = true
	return nil
}

// reactionKey is the Matrix reaction key for an slk emoji name: the
// standard emoji's glyph (with its skin tone), else the name itself.
func reactionKey(name string) string {
	name = strings.Trim(name, ":")
	if cps, ok := emoji.CodepointsForShortcode(name); ok {
		return string(cps)
	}
	if cps, ok := emoji.ComposeSkinTonedCodepoints(name); ok {
		return string(cps)
	}
	return name
}

// react is ReactionService.Add. Reactions are not encrypted (mautrix
// sends m.reaction in the clear, as the relation must be readable).
func (b *Backend) react(alias, ts, name string) error {
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return err
	}
	defer cancel()
	target, err := b.eventAtTS(ctx, room, ts)
	if err != nil {
		return err
	}
	key := reactionKey(name)
	resp, err := b.client.SendReaction(ctx, room, target, key)
	if err != nil {
		return err
	}
	w := b.write
	w.mu.Lock()
	w.reactions[reactionRef{room, target, reactionName(key)}] = resp.EventID
	w.mu.Unlock()
	return nil
}

// unreact is ReactionService.Remove: redacts the account's reaction with
// that emoji (compared by slk name, so a key with or without variation
// selectors matches). None found is not an error.
func (b *Backend) unreact(alias, ts, name string) error {
	ctx, cancel, room, err := b.writeCtx(alias)
	if err != nil {
		return err
	}
	defer cancel()
	target, err := b.eventAtTS(ctx, room, ts)
	if err != nil {
		return err
	}
	want := reactionName(reactionKey(name))
	var found []id.EventID
	rels, err := b.d.Store.Relations(ctx, room, target, string(event.RelAnnotation))
	if err != nil {
		return err
	}
	for _, e := range rels {
		if e.Sender != b.self || e.Redacted {
			continue
		}
		var c event.ReactionEventContent
		if json.Unmarshal(e.Content, &c) == nil && reactionName(c.RelatesTo.Key) == want {
			found = append(found, e.ID)
		}
	}
	w := b.write
	ref := reactionRef{room, target, want}
	w.mu.Lock()
	if ev, ok := w.reactions[ref]; ok && !slices.Contains(found, ev) {
		found = append(found, ev)
	}
	delete(w.reactions, ref)
	w.mu.Unlock()
	for _, ev := range found {
		if _, err := b.client.RedactEvent(ctx, room, ev); err != nil {
			return err
		}
	}
	return nil
}
