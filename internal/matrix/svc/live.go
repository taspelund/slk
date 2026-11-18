package svc

import (
	"context"
	"maps"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	tea "charm.land/bubbletea/v2"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/config"
	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/debuglog"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/notify"
	"github.com/gammons/slk/internal/slackfmt"
	"github.com/gammons/slk/internal/ui"
	"github.com/gammons/slk/internal/ui/sidebar"
)

// The live side (WP-Live): what the session syncs, applied to the room
// model and told to the UI, as cmd/slk's rtmEventHandler does for Slack's
// WebSocket.
//
// Startup. Run applies events from the start, but sends nothing until
// StartupMsgs has returned: after it applies the first sync (or the first
// sync fails, for an offline start from the store), it stops reading
// events until StartupMsgs has built the workspace from the model. So a
// caller that sends StartupMsgs' messages before letting Run's sends
// through, e.g.
//
//	gate := make(chan struct{})
//	go b.Run(ctx, s.Events(), func(m tea.Msg) { <-gate; p.Send(m) })
//	msgs, err := b.StartupMsgs(ctx)
//	for _, m := range msgs { p.Send(m) }
//	close(gate)
//
// delivers the workspace before any live message, and loses nothing in
// between. The batch that made the startup snapshot sends no messages:
// the snapshot already holds it.
//
// What each change becomes (rooms.Changes; one account is one workspace,
// always the active one):
//
//	rooms joined or changed  ConversationOpenedMsg (sidebar and finder row) and
//	                         ChannelResolvedMsg (mention name) per room, then
//	                         one SectionsRefreshedMsg with the whole sidebar
//	                         (a room left drops out, sections move)
//	names                    UserResolvedMsg
//	Added                    NewMessageMsg; a thread reply carries ThreadTS,
//	                         and reduceNewMessage bumps its parent's reply
//	                         count and users (IncrementReplyCount)
//	Edited                   NewMessageMsg{Changed: true}, IsEdited from the item
//	Redacted                 WSMessageDeletedMsg, or for a thread root with
//	                         replies NewMessageMsg{Changed: true} with the
//	                         tombstone
//	ReactionAdded/Removed    ReactionAddedMsg/ReactionRemovedMsg (Slack
//	                         shortcode, as items.go names reactions)
//	own receipt              ChannelMarkedRemoteMsg + ReadStateChangedMsg, or
//	                         for a thread ThreadMarkedRemoteMsg +
//	                         ThreadsListDirtyMsg; the UI tells slk's own marks
//	                         from other devices' (selfMarks)
//	other read-state change  ReadStateChangedMsg, except for a room this batch
//	                         sent a new message for: reduceNewMessage decides
//	                         whether that one is seen
//	typing                   UserTypingMsg per typer, repeated every
//	                         typingRefresh while they type (the UI forgets a
//	                         typer typingExpiry after the last one)
//	connection               ConnectionStateMsg (0 connecting, 1 connected,
//	                         2 disconnected)
//
// Backfilled changes (Live false) send nothing.
//
// The account's own messages are sent like anyone's, as Slack's echoes
// are: a message this client sent has the TS of its send response, which
// the UI's self-send guard (selfSend.IsSelfSent, and InFlight before the
// response) drops; one sent from another device is shown.
//
// Notifications follow rtmEventHandler.OnMessage: notify.ShouldNotify on
// every new live message after startup (never the account's own, the
// conversation on screen, or a change), titled "<account>: #room" or
// "<account>: <sender>" for a DM. A message that is still encrypted waits
// for its decryption to notify.

// typingRefresh is how often a typer still typing is told to the UI
// again; less than the UI's typingExpiry (5s).
var typingRefresh = 3 * time.Second

// Notifier is what delivers a notification: *notify.Notifier.
type Notifier interface {
	Notify(title, body string) error
}

// Notifications is how the backend notifies of new messages, from
// [notifications] in the config. ActiveChannelID is the conversation on
// screen (App.ActiveChannelID); nil is none.
type Notifications struct {
	Notifier        Notifier
	Config          config.Notifications
	ActiveChannelID func() string
}

// liveState is the live side's state of one Backend (Backend.live).
type liveState struct {
	mu    sync.Mutex
	notif Notifications

	// ready is closed when Run has applied the first sync, or seen the
	// first one fail; started when StartupMsgs has built its messages.
	ready, started         chan struct{}
	readyOnce, startedOnce sync.Once
	// synced says the first sync succeeded (set before ready closes).
	synced atomic.Bool

	sections *sectionsProvider
}

func newLiveState() *liveState {
	return &liveState{ready: make(chan struct{}), started: make(chan struct{}), sections: &sectionsProvider{}}
}

// SetNotifications sets how new messages notify. Call it before Run.
func (b *Backend) SetNotifications(n Notifications) {
	l := b.live
	l.mu.Lock()
	l.notif = n
	l.mu.Unlock()
}

// sectionsProvider is the sidebar's SectionsProvider: the room model's
// sections, replaced whole by Run when rooms change and read by the UI.
type sectionsProvider struct {
	p atomic.Pointer[[]sidebar.SectionMeta]
}

func (s *sectionsProvider) set(secs []sidebar.SectionMeta) {
	secs = slices.Clone(secs)
	s.p.Store(&secs)
}

func (s *sectionsProvider) Ready() bool { return s.p.Load() != nil }

func (s *sectionsProvider) OrderedSlackSections() []sidebar.SectionMeta {
	if p := s.p.Load(); p != nil {
		return slices.Clone(*p)
	}
	return nil
}

var _ sidebar.SectionsProvider = (*sectionsProvider)(nil)

// selfAlias is the account's user alias, the UI's current user ID.
func (b *Backend) selfAlias() string { return b.teamID[1:] }

// StartupMsgs waits until Run has applied the first sync, then returns
// the workspace as the UI starts with it: one WorkspaceReadyMsg (Theme
// and SidebarWidth left for the caller to fill from the config), then a
// ConnectionStateMsg. If the first sync fails, it does not wait for a
// later one: the workspace is the store's, and the connection
// disconnected.
func (b *Backend) StartupMsgs(ctx context.Context) ([]tea.Msg, error) {
	l := b.live
	select {
	case <-l.ready:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer l.startedOnce.Do(func() { close(l.started) })
	snap := b.rooms.Snapshot()
	l.sections.set(snap.Sections)
	name := b.d.Account
	if name == "" {
		name = b.self.Homeserver()
	}
	state := 2
	if l.synced.Load() {
		state = 1
	}
	return []tea.Msg{
		ui.WorkspaceReadyMsg{
			TeamID:           b.teamID,
			TeamName:         name,
			Channels:         snap.Channels,
			ChannelNames:     snap.ChannelNames,
			FinderItems:      snap.Finder,
			UserNames:        snap.UserNames,
			UserID:           b.selfAlias(),
			SectionsProvider: l.sections,
			InitialActive:    true,
			LastChannelID:    b.lastChannel(ctx, snap),
		},
		ui.ConnectionStateMsg{State: state},
	}, nil
}

// lastChannel is the sidebar room with the newest event.
func (b *Backend) lastChannel(ctx context.Context, snap rooms.Snapshot) string {
	rows, err := b.d.Store.ListRooms(ctx)
	if err != nil {
		return ""
	}
	inSidebar := map[string]bool{}
	for _, c := range snap.Channels {
		inSidebar[c.ID] = true
	}
	best, bestTS := "", ""
	for _, row := range rows {
		r, ok := b.rooms.RoomByID(row.ID)
		if ok && inSidebar[r.Alias] && row.LastEventTS > bestTS {
			best, bestTS = r.Alias, row.LastEventTS
		}
	}
	return best
}

// Run applies the session's events to the room model and sends what they
// change to the UI through send, until events is closed (nil) or ctx is
// done (its error). See the top of this file for the startup handshake
// with StartupMsgs and for what each change sends. An event the model
// fails to apply is logged and skipped.
func (b *Backend) Run(ctx context.Context, events <-chan session.Event, send func(tea.Msg)) error {
	l := b.live
	r := &liveRun{b: b, l: l, send: send, typing: map[id.RoomID][]id.UserID{}, undecrypted: map[id.EventID]bool{}}
	var tick <-chan time.Time
	var ticker *time.Ticker
	defer func() {
		if ticker != nil {
			ticker.Stop()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick:
			r.refreshTyping()
		case ev, ok := <-events:
			if !ok {
				return nil
			}
			if err := r.handle(ctx, ev); err != nil {
				return err
			}
		}
		switch {
		case len(r.typing) > 0 && ticker == nil:
			ticker = time.NewTicker(typingRefresh)
			tick = ticker.C
		case len(r.typing) == 0 && ticker != nil:
			ticker.Stop()
			ticker, tick = nil, nil
		}
	}
}

// liveRun is one Run's state; only Run's goroutine touches it.
type liveRun struct {
	b       *Backend
	l       *liveState
	send    func(tea.Msg)
	started bool
	// typing is who types where now, by room.
	typing map[id.RoomID][]id.UserID
	// undecrypted are new messages that arrived encrypted after startup,
	// to notify of once decrypted.
	undecrypted map[id.EventID]bool
}

// handle takes one session event. It returns an error only when ctx ends
// while waiting for StartupMsgs.
func (r *liveRun) handle(ctx context.Context, ev session.Event) error {
	switch ev := ev.(type) {
	case session.SyncBatch:
		if ev.Resp == nil {
			return nil
		}
		ch, err := r.b.rooms.Apply(ctx, ev)
		if err != nil {
			debuglog.General("matrix live: applying a sync: %v", err)
			return nil
		}
		if !r.started {
			r.l.synced.Store(true)
			return r.awaitStartup(ctx)
		}
		r.changes(ctx, ch, !ev.Initial, false)
	case session.LateDecrypted:
		ch, err := r.b.rooms.ApplyDecrypted(ctx, ev)
		if err != nil {
			debuglog.General("matrix live: applying decrypted events of %s: %v", ev.Room, err)
			return nil
		}
		if r.started {
			r.changes(ctx, ch, true, true)
		}
	case session.Connection:
		if !r.started {
			if ev.State == session.Disconnected {
				return r.awaitStartup(ctx)
			}
			return nil
		}
		r.send(ui.ConnectionStateMsg{State: int(ev.State)})
	}
	return nil
}

// awaitStartup opens StartupMsgs and waits until it has built the
// workspace.
func (r *liveRun) awaitStartup(ctx context.Context) error {
	r.l.readyOnce.Do(func() { close(r.l.ready) })
	select {
	case <-r.l.started:
		r.started = true
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// changes sends what ch changed. notifyNew is false for a batch whose
// new messages must not notify (an initial sync after an offline start);
// decrypted says ch is a LateDecrypted's.
//
// A late-decrypted message keeps its place: m.relates_to is cleartext in
// an encrypted event, so its thread was known (and an edit or reaction
// never shown) while it was still encrypted.
func (r *liveRun) changes(ctx context.Context, ch rooms.Changes, notifyNew, decrypted bool) {
	b := r.b
	res := b.rooms.Resolver()
	var snap *rooms.Snapshot
	snapshot := func() rooms.Snapshot {
		if snap == nil {
			s := b.rooms.Snapshot()
			snap = &s
		}
		return *snap
	}

	// Rooms first, so a new room's row exists before its messages.
	changed := uniqueRooms(ch.Rooms, ch.Joined, ch.Left)
	if len(changed) > 0 {
		s := snapshot()
		for _, rid := range changed {
			room, ok := b.rooms.RoomByID(rid)
			if !ok {
				continue
			}
			if i := slices.IndexFunc(s.Channels, func(c sidebar.ChannelItem) bool { return c.ID == room.Alias }); i >= 0 {
				msg := ui.ConversationOpenedMsg{TeamID: b.teamID, Item: s.Channels[i]}
				if j := slices.IndexFunc(s.Finder, func(f core.ChannelFinderItem) bool { return f.ID == room.Alias }); j >= 0 {
					msg.FinderItem = s.Finder[j]
				}
				r.send(msg)
			}
			if room.Name != "" {
				r.send(ui.ChannelResolvedMsg{TeamID: b.teamID, ChannelID: room.Alias, Name: room.Name})
			}
		}
		r.l.sections.set(s.Sections)
		r.send(ui.SectionsRefreshedMsg{TeamID: b.teamID, Channels: s.Channels})
	}
	for _, u := range ch.Names {
		r.send(ui.UserResolvedMsg{TeamID: b.teamID, UserID: res.UserAlias(u), DisplayName: b.rooms.DisplayName("", u)})
	}

	newIn := map[id.RoomID]bool{}
	for _, c := range ch.Timeline {
		if !c.Live {
			continue
		}
		room, ok := b.rooms.RoomByID(c.Room)
		if !ok {
			continue
		}
		switch c.Kind {
		case rooms.Added, rooms.Edited, rooms.Redacted:
			e, ok, err := b.d.Store.GetEvent(ctx, c.Room, c.Event)
			if err != nil || !ok {
				continue
			}
			it, shown := r.item(ctx, c.Room, e)
			switch {
			case c.Kind == rooms.Redacted && !shown:
				r.send(ui.WSMessageDeletedMsg{ChannelID: room.Alias, TS: c.TS})
			case !shown:
			case c.Kind == rooms.Added:
				r.send(ui.NewMessageMsg{ChannelID: room.Alias, Message: it})
				newIn[c.Room] = true
				if notifyNew {
					r.notify(c.Room, room, e.ID, it)
				}
			default:
				r.send(ui.NewMessageMsg{ChannelID: room.Alias, Message: it, Changed: true})
				if c.Kind == rooms.Edited && decrypted {
					r.notifyDecrypted(c.Room, room, e.ID, it)
				}
			}
		case rooms.ReactionAdded, rooms.ReactionRemoved:
			ts := c.TS
			user := res.UserAlias(c.Sender)
			name := reactionName(c.Key)
			if c.Kind == rooms.ReactionAdded {
				r.send(ui.ReactionAddedMsg{ChannelID: room.Alias, MessageTS: ts, UserID: user, Emoji: name})
			} else {
				r.send(ui.ReactionRemovedMsg{ChannelID: room.Alias, MessageTS: ts, UserID: user, Emoji: name})
			}
		}
	}

	// Read state: own receipts as Slack's *_marked events, the rest as a
	// plain refresh.
	refreshed := map[id.RoomID]bool{}
	for _, rec := range ch.OwnReceipts {
		room, ok := b.rooms.RoomByID(rec.Room)
		if !ok {
			continue
		}
		if rec.Thread == "" {
			unread := 0
			if snapshot().ReadStates[room.Alias].HasUnread {
				unread = 1
			}
			r.send(ui.ReadStateChangedMsg{WorkspaceID: b.teamID, ChannelID: room.Alias})
			r.send(ui.ChannelMarkedRemoteMsg{ChannelID: room.Alias, TS: rec.TS, UnreadCount: unread})
			refreshed[rec.Room] = true
			continue
		}
		rootTS, ok, err := b.d.IDs.TS(ctx, rec.Room, rec.Thread)
		if err != nil || !ok {
			continue
		}
		r.send(ui.ThreadMarkedRemoteMsg{ChannelID: room.Alias, ThreadTS: rootTS, LastRead: rec.TS})
		r.send(ui.ThreadsListDirtyMsg{TeamID: b.teamID})
	}
	for _, rid := range ch.ReadStates {
		if refreshed[rid] || newIn[rid] {
			continue
		}
		refreshed[rid] = true
		if room, ok := b.rooms.RoomByID(rid); ok {
			r.send(ui.ReadStateChangedMsg{WorkspaceID: b.teamID, ChannelID: room.Alias})
		}
	}

	// Typing, in room order so what is sent does not depend on map order.
	for _, rid := range slices.Sorted(maps.Keys(ch.Typing)) {
		users := ch.Typing[rid]
		if len(users) == 0 {
			delete(r.typing, rid)
			continue
		}
		r.typing[rid] = slices.Clone(users)
		r.sendTyping(rid, users)
	}
}

// refreshTyping tells the UI again who is still typing.
func (r *liveRun) refreshTyping() {
	for _, rid := range slices.Sorted(maps.Keys(r.typing)) {
		r.sendTyping(rid, r.typing[rid])
	}
}

func (r *liveRun) sendTyping(rid id.RoomID, users []id.UserID) {
	room, ok := r.b.rooms.RoomByID(rid)
	if !ok {
		return
	}
	res := r.b.rooms.Resolver()
	for _, u := range users {
		r.send(ui.UserTypingMsg{ChannelID: room.Alias, UserID: res.UserAlias(u), WorkspaceID: r.b.teamID})
	}
}

// item is stored event e as the UI shows it, false if it is not shown.
func (r *liveRun) item(ctx context.Context, rid id.RoomID, e store.Event) (core.MessageItem, bool) {
	if !isRow(e) {
		return core.MessageItem{}, false
	}
	its, err := r.b.items(ctx, rid, []store.Event{e})
	if err != nil {
		debuglog.General("matrix live: message %s in %s: %v", e.ID, rid, err)
		return core.MessageItem{}, false
	}
	if len(its) != 1 {
		return core.MessageItem{}, false
	}
	return its[0], true
}

// notify notifies of a new message when the config says so, as
// rtmEventHandler.OnMessage does. One still encrypted waits for its
// decryption (notifyDecrypted).
func (r *liveRun) notify(rid id.RoomID, room rooms.Room, ev id.EventID, it core.MessageItem) {
	if it.Text == undecryptableText {
		if it.UserID != r.b.selfAlias() {
			r.undecrypted[ev] = true
		}
		return
	}
	r.l.mu.Lock()
	n := r.l.notif
	r.l.mu.Unlock()
	if n.Notifier == nil || !n.Config.Enabled {
		return
	}
	active := ""
	if n.ActiveChannelID != nil {
		active = n.ActiveChannelID()
	}
	nctx := notify.NotifyContext{
		CurrentUserID:   r.b.selfAlias(),
		ActiveChannelID: active,
		IsActiveWS:      true,
		OnMention:       n.Config.OnMention,
		OnDM:            n.Config.OnDM,
		OnKeyword:       n.Config.OnKeyword,
	}
	kind := string(room.Kind)
	if !notify.ShouldNotify(nctx, room.Alias, it.UserID, it.Text, kind) {
		return
	}
	title := r.b.workspaceName() + ": #" + room.Name
	if room.Kind == store.RoomDM || room.Kind == store.RoomGroupDM {
		title = r.b.workspaceName() + ": " + it.UserName
	}
	// Just the mentioned names, as rtmEventHandler does.
	res := r.b.rooms.Resolver()
	names := map[string]string{}
	for _, a := range slackfmt.MentionedUserIDs(it.Text) {
		if u, ok := res.MXID(a); ok {
			names[a] = r.b.rooms.DisplayName(rid, u)
		}
	}
	body := it.UserName + ": " + notify.StripSlackMarkup(it.Text, names)
	go func() {
		if err := n.Notifier.Notify(title, body); err != nil {
			debuglog.Notify("notification failed: %v", err)
		}
	}()
}

// notifyDecrypted notifies of a message that arrived encrypted after
// startup and is now decrypted.
func (r *liveRun) notifyDecrypted(rid id.RoomID, room rooms.Room, ev id.EventID, it core.MessageItem) {
	if !r.undecrypted[ev] {
		return
	}
	delete(r.undecrypted, ev)
	r.notify(rid, room, ev, it)
}

// workspaceName is the account's name in notification titles.
func (b *Backend) workspaceName() string {
	if b.d.Account != "" {
		return b.d.Account
	}
	return b.self.Homeserver()
}

// uniqueRooms is the rooms of lists, each once, in first-seen order.
func uniqueRooms(lists ...[]id.RoomID) []id.RoomID {
	var out []id.RoomID
	seen := map[id.RoomID]bool{}
	for _, l := range lists {
		for _, rid := range l {
			if !seen[rid] {
				seen[rid] = true
				out = append(out, rid)
			}
		}
	}
	return out
}
