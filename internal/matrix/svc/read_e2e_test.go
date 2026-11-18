//go:build matrixe2e

package svc

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// syncing is alice's device syncing into a room model, with a Backend
// over it, until stop.
type syncing struct {
	b    *Backend
	m    *rooms.Model
	stop func()
}

func startSyncing(t *testing.T, creds session.Credentials, st *store.Store, ids *ident.Mapper) *syncing {
	t.Helper()
	ctx := e2e.Ctx(t)
	s, err := session.New(ctx, creds, st, zerolog.Nop())
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	m, err := rooms.New(ctx, st, ids, creds.UserID)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	ran, applied := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(ran)
		if err := s.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("session Run: %v", err)
		}
	}()
	go func() {
		defer close(applied)
		for ev := range s.Events() {
			var err error
			switch ev := ev.(type) {
			case session.SyncBatch:
				_, err = m.Apply(runCtx, ev)
			case session.LateDecrypted:
				_, err = m.ApplyDecrypted(runCtx, ev)
			}
			if err != nil && runCtx.Err() == nil {
				t.Errorf("applying %T: %v", ev, err)
			}
		}
	}()
	var stopped bool
	stop := func() {
		if !stopped {
			stopped = true
			cancel()
			<-ran
			<-applied
		}
	}
	t.Cleanup(stop)
	return &syncing{b: New(Deps{Account: "e2e", Session: s, Store: st, IDs: ids, Rooms: m}), m: m, stop: stop}
}

// TestReadAgainstHomeserver pages a room's history through the read ports
// while alice's device syncs: more than a page before a restart, more than
// a /sync timeline while it is stopped (a gap), and a thread with a reply
// inside the gap.
func TestReadAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	ctx := e2e.Ctx(t)

	creds, err := session.Login(ctx, srv.URL, aliceID.String(), password, "slk (read e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	ids := ident.New(st)
	stored := func(room id.RoomID, ev id.EventID) bool {
		_, ok, err := st.GetEvent(context.Background(), room, ev)
		return err == nil && ok
	}

	first := startSyncing(t, creds, st, ids)
	room := bob.CreateRoom(t, e2e.RoomOptions{Name: "history", Invite: []id.UserID{aliceID}})
	if _, err := first.b.client.JoinRoomByID(ctx, room); err != nil {
		t.Fatalf("join: %v", err)
	}
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)

	// Phase one, synced: p000..p059, with a thread on p005.
	var want []string
	var root, last id.EventID
	for i := range 60 {
		body := fmt.Sprintf("p%03d", i)
		last = bob.SendText(t, room, body)
		want = append(want, body)
		if i == 5 {
			root = last
			bob.SendThreadReply(t, room, root, "reply one")
			bob.SendThreadReply(t, room, root, "reply two")
		}
	}
	e2e.Eventually(t, "phase one synced", func() bool { return stored(room, last) })
	first.stop()

	// Phase two, while alice is away: q000..q069, more than the /sync
	// timeline limit, with a third thread reply among the first ones.
	var inGap id.EventID
	for i := range 70 {
		body := fmt.Sprintf("q%03d", i)
		last = bob.SendText(t, room, body)
		want = append(want, body)
		if i == 3 {
			inGap = bob.SendThreadReply(t, room, root, "reply three")
		}
	}
	second := startSyncing(t, creds, st, ids)
	b := second.b
	e2e.Eventually(t, "phase two synced", func() bool { return stored(room, last) })
	gaps, err := st.Gaps(ctx, room)
	if err != nil || len(gaps) == 0 {
		t.Fatalf("no gap after the restart: %+v %v", gaps, err)
	}
	if stored(room, inGap) {
		t.Fatalf("the reply in the gap was synced")
	}
	alias := second.m.Resolver().RoomAlias(room)

	// The thread first: its reply in the gap is fetched and placed.
	rootTS, ok, err := ids.TS(ctx, room, root)
	if err != nil || !ok {
		t.Fatalf("root TS: %v %v", ok, err)
	}
	thread := b.fetchThread(alias, rootTS).(ui.ThreadRepliesLoadedMsg)
	if got := texts(thread.Replies); !slices.Equal(got, []string{"reply one", "reply two", "reply three"}) {
		t.Fatalf("thread replies = %q", got)
	}
	assertAscending(t, thread.Replies)

	// The newest page, then older pages to the start.
	loaded := b.fetch(alias, nil).(ui.MessagesLoadedMsg)
	if len(loaded.Messages) != pageSize {
		t.Fatalf("Fetch: %d messages", len(loaded.Messages))
	}
	all := loaded.Messages
	for range 10 {
		anchor := all[0].TS
		older := b.fetchOlder(alias, anchor).(ui.OlderMessagesLoadedMsg)
		if older.AnchorTS != anchor {
			t.Fatalf("AnchorTS = %q, want %q", older.AnchorTS, anchor)
		}
		if len(older.Messages) == 0 {
			break
		}
		if last := older.Messages[len(older.Messages)-1].TS; last >= anchor {
			t.Fatalf("page ends at %s, not before %s", last, anchor)
		}
		all = append(slices.Clone(older.Messages), all...)
	}
	assertAscending(t, all)
	if got := texts(all); !slices.Equal(got, want) {
		t.Fatalf("history (%d) = %q\nwant (%d) %q", len(got), got, len(want), want)
	}
	if i := slices.IndexFunc(all, func(it core.MessageItem) bool { return it.TS == rootTS }); i < 0 || all[i].ReplyCount != 3 {
		t.Fatalf("thread root in the history: %+v", all[max(i, 0)])
	}
	if gaps, err := st.Gaps(ctx, room); err != nil || len(gaps) != 0 {
		t.Fatalf("gaps after paging back = %+v %v", gaps, err)
	}

	// Search finds a message from the gap.
	res := b.searchChannel(alias, "q007").(ui.ChannelSearchResultsMsg)
	if res.Err != nil || len(res.TSes) != 1 || res.TSes[0] != all[slices.Index(want, "q007")].TS {
		t.Fatalf("search = %+v", res)
	}
}
