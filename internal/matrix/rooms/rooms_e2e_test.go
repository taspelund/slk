//go:build matrixe2e

package rooms

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
)

// liveModel is alice's slk device: a session whose events a Model applies
// as they arrive, recording every Changes.
type liveModel struct {
	m  *Model
	st *store.Store

	mu      sync.Mutex
	changes []Changes
}

func startLive(t *testing.T, srv e2e.Server, user id.UserID, password string) *liveModel {
	t.Helper()
	ctx := e2e.Ctx(t)
	creds, err := session.Login(ctx, srv.URL, user.String(), password, "slk (rooms e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := session.New(ctx, creds, st, zerolog.Nop())
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	m, err := New(ctx, st, ident.New(st), user)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	l := &liveModel{m: m, st: st}

	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := s.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("session Run: %v", err)
		}
	}()
	consumed := make(chan struct{})
	go func() {
		defer close(consumed)
		for ev := range s.Events() {
			var ch Changes
			var err error
			switch ev := ev.(type) {
			case session.SyncBatch:
				ch, err = m.Apply(runCtx, ev)
			case session.LateDecrypted:
				ch, err = m.ApplyDecrypted(runCtx, ev)
			default:
				continue
			}
			if err != nil {
				if runCtx.Err() == nil {
					t.Errorf("applying %T: %v", ev, err)
				}
				continue
			}
			l.mu.Lock()
			l.changes = append(l.changes, ch)
			l.mu.Unlock()
		}
	}()
	// Registered after the store's cleanup, so it runs first.
	t.Cleanup(func() {
		cancel()
		<-done
		<-consumed
	})
	return l
}

// timeline returns every timeline change so far.
func (l *liveModel) timeline() []TimelineChange {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []TimelineChange
	for _, ch := range l.changes {
		out = append(out, ch.Timeline...)
	}
	return out
}

func (l *liveModel) ownReceipts() []Receipt {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []Receipt
	for _, ch := range l.changes {
		out = append(out, ch.OwnReceipts...)
	}
	return out
}

// hasChange reports whether a change like want (ignoring TS) arrived.
func (l *liveModel) hasChange(want TimelineChange) bool {
	want.TS = ""
	return slices.ContainsFunc(l.timeline(), func(c TimelineChange) bool {
		c.TS = ""
		return c == want
	})
}

// createSpace creates a space as peer, inviting invite.
func createSpace(t *testing.T, peer *e2e.Peer, name string, invite ...id.UserID) id.RoomID {
	t.Helper()
	resp, err := peer.Client.CreateRoom(e2e.Ctx(t), &mautrix.ReqCreateRoom{
		Name:            name,
		Invite:          invite,
		Preset:          "private_chat",
		CreationContent: map[string]any{"type": "m.space"},
	})
	if err != nil {
		t.Fatalf("create space: %v", err)
	}
	return resp.RoomID
}

// addChild puts child in space as peer.
func addChild(t *testing.T, peer *e2e.Peer, space, child id.RoomID) {
	t.Helper()
	_, err := peer.Client.SendStateEvent(e2e.Ctx(t), space, event.StateSpaceChild, child.String(),
		&event.SpaceChildEventContent{Via: []string{peer.UserID.Homeserver()}})
	if err != nil {
		t.Fatalf("add %s to %s: %v", child, space, err)
	}
}

// createPublicRoom creates a public room as peer.
func createPublicRoom(t *testing.T, peer *e2e.Peer, name string, invite ...id.UserID) id.RoomID {
	t.Helper()
	resp, err := peer.Client.CreateRoom(e2e.Ctx(t), &mautrix.ReqCreateRoom{Name: name, Invite: invite, Preset: "public_chat"})
	if err != nil {
		t.Fatalf("create room: %v", err)
	}
	return resp.RoomID
}

func TestLiveModelAgainstHomeserver(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	l := startLive(t, srv, aliceID, password)
	// alice's second device: joins, writes m.direct and sends a receipt.
	alice2 := e2e.LoginPeer(t, srv, aliceID, password)

	space := createSpace(t, bob, "Team", aliceID)
	general := createPublicRoom(t, bob, "general", aliceID)
	random := bob.CreateRoom(t, e2e.RoomOptions{Name: "random", Invite: []id.UserID{aliceID}})
	addChild(t, bob, space, general)
	addChild(t, bob, space, random)
	dm := bob.CreateDM(t, aliceID, true)
	for _, room := range []id.RoomID{space, general, random, dm} {
		alice2.Join(t, room)
		bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	}
	if err := alice2.Client.SetAccountData(e2e.Ctx(t), event.AccountDataDirectChats.Type,
		event.DirectChatsEventContent{bob.UserID: {dm}}); err != nil {
		t.Fatalf("m.direct: %v", err)
	}

	m1 := bob.SendText(t, general, "hello team")
	bob.SendEdit(t, general, m1, "hello everyone")
	bob.React(t, general, m1, "👍")
	reply := bob.SendThreadReply(t, general, m1, "in a thread")
	secret := bob.SendText(t, dm, "secret hi")

	e2e.Eventually(t, "the messages in alice's model", func() bool {
		return l.hasChange(TimelineChange{Kind: Edited, Room: general, Event: m1, Live: true}) &&
			l.hasChange(TimelineChange{Kind: ReactionAdded, Room: general, Event: m1, Key: "👍", Sender: bob.UserID, Live: true}) &&
			l.hasChange(TimelineChange{Kind: Added, Room: general, Event: reply, Thread: m1, Live: true})
	})
	e2e.Eventually(t, "the DM message decrypted", func() bool {
		e, ok, err := l.st.GetEvent(context.Background(), dm, secret)
		return err == nil && ok && e.Decrypted && e.Body == "secret hi"
	})
	if !l.hasChange(TimelineChange{Kind: Added, Room: general, Event: m1, Live: true}) {
		t.Errorf("no Added change for %s: %+v", m1, l.timeline())
	}
	if e, _, _ := l.st.GetEvent(context.Background(), general, m1); e.Body != "hello everyone" {
		t.Errorf("edited body = %q", e.Body)
	}

	e2e.Eventually(t, "the sidebar", func() bool {
		snap := l.m.Snapshot()
		return len(snap.Channels) == 3 && len(snap.Sections) == 3
	})
	snap := l.m.Snapshot()
	spaceAlias := l.m.Resolver().RoomAlias(space)
	if snap.Sections[0].ID != spaceAlias || snap.Sections[0].Name != "Team" || snap.Sections[0].Type != "standard" ||
		snap.Sections[1].ID != "rooms" || snap.Sections[2].ID != "dms" {
		t.Fatalf("Sections = %+v", snap.Sections)
	}
	bobName := l.m.DisplayName(dm, bob.UserID)
	type row struct{ name, typ, section, dm string }
	var got []row
	for _, c := range snap.Channels {
		got = append(got, row{c.Name, c.Type, c.Section, c.DMUserID})
	}
	want := []row{
		{"general", "channel", spaceAlias, ""},
		{"random", "private", spaceAlias, ""},
		{bobName, "dm", "dms", l.m.Resolver().UserAlias(bob.UserID)},
	}
	if !slices.Equal(got, want) {
		t.Fatalf("Channels = %+v\nwant %+v", got, want)
	}
	if r, ok := l.m.RoomByID(dm); !ok || !r.Encrypted || r.DMUser != bob.UserID {
		t.Fatalf("dm = %+v %v", r, ok)
	}
	generalAlias := l.m.Resolver().RoomAlias(general)
	if rs := snap.ReadStates[generalAlias]; !rs.HasUnread {
		t.Fatalf("general before the receipt: %+v", rs)
	}

	// A receipt from alice's other device marks general read (the thread
	// reply, edit and reaction do not count).
	alice2.ReadReceipt(t, general, m1)
	e2e.Eventually(t, "alice's own receipt", func() bool {
		return slices.ContainsFunc(l.ownReceipts(), func(r Receipt) bool {
			return r.Room == general && r.Event == m1 && r.Thread == "" && r.TS != ""
		})
	})
	snap = l.m.Snapshot()
	if rs := snap.ReadStates[generalAlias]; rs.HasUnread || rs.LastReadTS == "" {
		t.Fatalf("general after the receipt: %+v", rs)
	}
	if rs := snap.ReadStates[l.m.Resolver().RoomAlias(dm)]; !rs.HasUnread {
		t.Fatalf("dm: %+v", rs)
	}
}
