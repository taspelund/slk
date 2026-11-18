//go:build matrixe2e

package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/store"
)

// runner runs a Session and collects its events.
type runner struct {
	s      *Session
	cancel context.CancelFunc
	done   chan error

	mu     sync.Mutex
	got    []Event
	closed bool
	notify chan struct{}
	next   int // first event waitFor has not looked at
}

func start(t *testing.T, s *Session) *runner {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r := &runner{s: s, cancel: cancel, done: make(chan error, 1), notify: make(chan struct{})}
	go func() { r.done <- s.Run(ctx) }()
	go func() {
		for ev := range s.Events() {
			r.mu.Lock()
			r.got = append(r.got, ev)
			close(r.notify)
			r.notify = make(chan struct{})
			r.mu.Unlock()
		}
		r.mu.Lock()
		r.closed = true
		close(r.notify)
		r.mu.Unlock()
	}()
	t.Cleanup(func() { r.stop() })
	return r
}

// stop cancels Run and returns its error (once).
func (r *runner) stop() error {
	r.cancel()
	err, ok := <-r.done
	if !ok {
		return nil
	}
	close(r.done)
	return err
}

// waitFor returns the first event after the last one waitFor returned that
// match accepts.
func (r *runner) waitFor(t *testing.T, what string, match func(Event) bool) Event {
	t.Helper()
	ctx := e2e.Ctx(t)
	for {
		r.mu.Lock()
		got, notify, closed := r.got, r.notify, r.closed
		r.mu.Unlock()
		for ; r.next < len(got); r.next++ {
			if match(got[r.next]) {
				r.next++
				return got[r.next-1]
			}
		}
		if closed {
			t.Fatalf("Events closed while waiting for %s", what)
		}
		select {
		case <-notify:
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		}
	}
}

func (r *runner) waitBatch(t *testing.T, what string, match func(SyncBatch) bool) SyncBatch {
	t.Helper()
	return r.waitFor(t, what, func(ev Event) bool {
		b, ok := ev.(SyncBatch)
		return ok && match(b)
	}).(SyncBatch)
}

// timelineEvent finds an event in room's timeline in resp.
func timelineEvent(resp *mautrix.RespSync, room id.RoomID, match func(*event.Event) bool) *event.Event {
	if r, ok := resp.Rooms.Join[room]; ok {
		for _, evt := range r.Timeline.Events {
			if match(evt) {
				return evt
			}
		}
	}
	return nil
}

func hasBody(body string) func(*event.Event) bool {
	return func(evt *event.Event) bool {
		return evt.Type == event.EventMessage && evt.Content.AsMessage().Body == body
	}
}

func hasID(evtID id.EventID) func(*event.Event) bool {
	return func(evt *event.Event) bool { return evt.ID == evtID }
}

func openStore(t *testing.T, path string) *store.Store {
	t.Helper()
	st, err := store.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// login logs a new slk device in for user, saves its credentials in a
// fresh account directory and reads them back.
func login(t *testing.T, srv e2e.Server, user id.UserID, password string) (Credentials, string) {
	t.Helper()
	creds, err := Login(e2e.Ctx(t), srv.URL, user.String(), password, "slk (e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	dir := filepath.Join(t.TempDir(), "acct")
	if err := SaveCredentials(dir, creds); err != nil {
		t.Fatalf("SaveCredentials: %v", err)
	}
	loaded, err := LoadCredentials(dir)
	if err != nil {
		t.Fatalf("LoadCredentials: %v", err)
	}
	return loaded, dir
}

func newSession(t *testing.T, creds Credentials, st *store.Store) *Session {
	t.Helper()
	log := zerolog.Nop()
	if os.Getenv("MATRIX_E2E_LOG") != "" {
		log = zerolog.New(zerolog.ConsoleWriter{Out: os.Stderr}).With().Timestamp().Logger()
	}
	s, err := New(e2e.Ctx(t), creds, st, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// joinedRoom has peer create a room (encrypted or not) with alice's
// session, joins it as alice, and waits until peer sees the join.
func joinedRoom(t *testing.T, peer *e2e.Peer, s *Session, encrypted bool) id.RoomID {
	t.Helper()
	room := peer.CreateRoom(t, e2e.RoomOptions{Encrypted: encrypted, Invite: []id.UserID{s.Client().UserID}})
	if _, err := s.Client().JoinRoomByID(e2e.Ctx(t), room); err != nil {
		t.Fatalf("join %s: %v", room, err)
	}
	peer.WaitMember(t, room, s.Client().UserID, event.MembershipJoin)
	return room
}

// TestLoginAndResume: a session started again from the saved credentials
// and database resumes from the saved sync token without logging in again.
func TestLoginAndResume(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	creds, dir := login(t, srv, aliceID, password)
	if creds.UserID != aliceID || creds.DeviceID == "" || len(creds.PickleKey) != 32 {
		t.Fatalf("credentials = %+v", creds)
	}
	dbPath := filepath.Join(t.TempDir(), "matrix.db")

	st := openStore(t, dbPath)
	s := newSession(t, creds, st)
	r := start(t, s)
	r.waitFor(t, "Connecting", func(ev Event) bool { return ev == Event(Connection{State: Connecting}) })
	r.waitFor(t, "Connected", func(ev Event) bool { return ev == Event(Connection{State: Connected}) })
	r.waitBatch(t, "the initial batch", func(b SyncBatch) bool { return b.Initial })

	room := joinedRoom(t, bob, s, false)
	bob.SendText(t, room, "before restart")
	b := r.waitBatch(t, "the first message", func(b SyncBatch) bool {
		return timelineEvent(b.Resp, room, hasBody("before restart")) != nil
	})
	if b.Initial {
		t.Error("a later batch is marked Initial")
	}
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Errorf("Run = %v, want context.Canceled", err)
	}
	saved, err := s.tokens.GetNextBatch(e2e.Ctx(t))
	if err != nil || saved == "" {
		t.Fatalf("saved sync token = %q, %v", saved, err)
	}
	if err := st.DB.Close(); err != nil {
		t.Fatal(err)
	}

	bob.SendText(t, room, "while stopped")

	creds2, err := LoadCredentials(dir)
	if err != nil {
		t.Fatal(err)
	}
	s2 := newSession(t, creds2, openStore(t, dbPath))
	if got, _ := s2.tokens.GetNextBatch(e2e.Ctx(t)); got != saved {
		t.Errorf("restarted session's token = %q, want %q", got, saved)
	}
	r2 := start(t, s2)
	first := r2.waitBatch(t, "the first batch after restart", func(SyncBatch) bool { return true })
	if !first.Initial {
		t.Error("first batch after restart is not marked Initial")
	}
	if timelineEvent(first.Resp, room, hasBody("before restart")) != nil {
		t.Error("restart delivered an event from before the saved token again")
	}
	if timelineEvent(first.Resp, room, func(evt *event.Event) bool { return evt.Type == event.StateCreate }) != nil {
		t.Error("restart did a full initial sync")
	}
	if timelineEvent(first.Resp, room, hasBody("while stopped")) == nil {
		// It may have come in a later batch if the server split them.
		r2.waitBatch(t, "the message sent while stopped", func(b SyncBatch) bool {
			return timelineEvent(b.Resp, room, hasBody("while stopped")) != nil
		})
	}
	if s2.Client().DeviceID != creds.DeviceID {
		t.Errorf("device = %s, want %s", s2.Client().DeviceID, creds.DeviceID)
	}
}

// TestEncryptedMessagesDecrypted: messages a peer sends to an encrypted
// room, of every kind the harness sends, arrive decrypted in a SyncBatch.
func TestEncryptedMessagesDecrypted(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	creds, _ := login(t, srv, aliceID, password)
	s := newSession(t, creds, openStore(t, filepath.Join(t.TempDir(), "matrix.db")))
	r := start(t, s)
	r.waitBatch(t, "the initial batch", func(b SyncBatch) bool { return b.Initial })
	room := joinedRoom(t, bob, s, true)

	decrypted := func(what string, evtID id.EventID) *event.Event {
		t.Helper()
		var got *event.Event
		r.waitBatch(t, what, func(b SyncBatch) bool {
			got = timelineEvent(b.Resp, room, hasID(evtID))
			return got != nil
		})
		return got
	}

	root := bob.SendText(t, room, "secret")
	evt := decrypted("the encrypted message", root)
	if evt.Type != event.EventMessage || evt.Content.AsMessage().Body != "secret" ||
		!evt.Mautrix.WasEncrypted || evt.Mautrix.EventSource&event.SourceDecrypted == 0 {
		t.Fatalf("got %s %+v (WasEncrypted %v), want a decrypted m.room.message", evt.Type, evt.Content.Parsed, evt.Mautrix.WasEncrypted)
	}

	html := bob.SendHTML(t, room, "bold", "<b>bold</b>")
	if c := decrypted("the HTML message", html).Content.AsMessage(); c.FormattedBody != "<b>bold</b>" || c.Format != event.FormatHTML {
		t.Errorf("HTML message = %+v", c)
	}
	thread := bob.SendThreadReply(t, room, root, "in thread")
	if rel := decrypted("the thread reply", thread).Content.AsMessage().RelatesTo; rel.GetThreadParent() != root {
		t.Errorf("thread reply relates to %+v", rel)
	}
	reply := bob.SendReply(t, room, root, "a reply")
	if rel := decrypted("the reply", reply).Content.AsMessage().RelatesTo; rel.GetReplyTo() != root {
		t.Errorf("reply relates to %+v", rel)
	}
	edit := bob.SendEdit(t, room, root, "edited")
	if c := decrypted("the edit", edit).Content.AsMessage(); c.RelatesTo.GetReplaceID() != root || c.NewContent.Body != "edited" {
		t.Errorf("edit = %+v", c)
	}
	react := bob.React(t, room, root, "👍")
	if rel := decrypted("the reaction", react).Content.AsReaction().RelatesTo; rel.Key != "👍" || rel.EventID != root {
		t.Errorf("reaction = %+v", rel)
	}
	redact := bob.Redact(t, room, html)
	if evt := decrypted("the redaction", redact); evt.Redacts != html && evt.Content.AsRedaction().Redacts != html {
		t.Errorf("redaction = %+v", evt)
	}
	bob.ReadReceipt(t, room, edit)
	r.waitBatch(t, "bob's read receipt", func(b SyncBatch) bool {
		for _, evt := range b.Resp.Rooms.Join[room].Ephemeral.Events {
			if evt.Type == event.EphemeralEventReceipt {
				if receipts := (*evt.Content.AsReceipt())[edit]; len(receipts[event.ReceiptTypeRead]) > 0 {
					return true
				}
			}
		}
		return false
	})
}

// TestLateKeyDelivered: an event whose room key reaches the device after
// the event itself is delivered encrypted, then as LateDecrypted.
func TestLateKeyDelivered(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	creds, _ := login(t, srv, aliceID, password)
	s := newSession(t, creds, openStore(t, filepath.Join(t.TempDir(), "matrix.db")))
	r := start(t, s)
	r.waitBatch(t, "the initial batch", func(b SyncBatch) bool { return b.Initial })
	room := joinedRoom(t, bob, s, true)

	late := bob.SendTextWithheld(t, room, "late")
	b := r.waitBatch(t, "the undecryptable event", func(b SyncBatch) bool {
		return timelineEvent(b.Resp, room, hasID(late)) != nil
	})
	if evt := timelineEvent(b.Resp, room, hasID(late)); evt.Type != event.EventEncrypted {
		t.Fatalf("event without its key arrived as %s, want m.room.encrypted", evt.Type)
	}

	bob.ShareRoomKey(t, room, aliceID)
	ld := r.waitFor(t, "LateDecrypted", func(ev Event) bool {
		_, ok := ev.(LateDecrypted)
		return ok
	}).(LateDecrypted)
	if ld.Room != room || len(ld.Events) != 1 || ld.Events[0].ID != late ||
		ld.Events[0].Content.AsMessage().Body != "late" {
		t.Fatalf("LateDecrypted = %s %v", ld.Room, ld.Events)
	}
}

// TestLimitedTimeline: more messages than the timeline limit while the
// session is stopped come back as a limited timeline with a prev_batch.
func TestLimitedTimeline(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	bob := e2e.NewPeer(t, srv, "bob")
	creds, _ := login(t, srv, aliceID, password)
	dbPath := filepath.Join(t.TempDir(), "matrix.db")
	st := openStore(t, dbPath)
	s := newSession(t, creds, st)
	r := start(t, s)
	r.waitBatch(t, "the initial batch", func(b SyncBatch) bool { return b.Initial })
	room := joinedRoom(t, bob, s, false)
	bob.SendText(t, room, "first")
	r.waitBatch(t, "the first message", func(b SyncBatch) bool {
		return timelineEvent(b.Resp, room, hasBody("first")) != nil
	})
	if err := r.stop(); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v", err)
	}
	if err := st.DB.Close(); err != nil {
		t.Fatal(err)
	}

	const n = timelineLimit + 10
	for i := range n {
		bob.SendText(t, room, fmt.Sprintf("msg %d", i))
	}

	r2 := start(t, newSession(t, creds, openStore(t, dbPath)))
	last := fmt.Sprintf("msg %d", n-1)
	b := r2.waitBatch(t, "the last message", func(b SyncBatch) bool {
		return timelineEvent(b.Resp, room, hasBody(last)) != nil
	})
	tl := b.Resp.Rooms.Join[room].Timeline
	if !tl.Limited || tl.PrevBatch == "" {
		t.Errorf("timeline limited=%v prev_batch=%q, want limited with a prev_batch", tl.Limited, tl.PrevBatch)
	}
	if len(tl.Events) > timelineLimit {
		t.Errorf("timeline has %d events, more than the limit %d", len(tl.Events), timelineLimit)
	}
	if timelineEvent(b.Resp, room, hasBody("msg 0")) != nil {
		t.Error("limited timeline still has the oldest message")
	}
}

// TestRecoveryKeyRestoresHistory: a second device verifies itself with the
// recovery key set up by the first, and decrypts from the key backup a
// message sent before it existed.
func TestRecoveryKeyRestoresHistory(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	alice1 := e2e.LoginPeer(t, srv, aliceID, password)
	recoveryKey := alice1.SetupRecovery(t)

	bob := e2e.NewPeer(t, srv, "bob")
	room := bob.CreateRoom(t, e2e.RoomOptions{Encrypted: true, Invite: []id.UserID{aliceID}})
	alice1.Join(t, room)
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	history := bob.SendText(t, room, "from before the second device")
	if evt := alice1.WaitText(t, room, "from before the second device"); !evt.Mautrix.WasEncrypted {
		t.Fatal("the first device did not decrypt the history message")
	}
	if n := alice1.BackupRoomKeys(t); n == 0 {
		t.Fatal("the first device backed up no room keys")
	}

	creds, _ := login(t, srv, aliceID, password)
	s := newSession(t, creds, openStore(t, filepath.Join(t.TempDir(), "matrix.db")))
	r := start(t, s)
	b := r.waitBatch(t, "the history message", func(b SyncBatch) bool {
		return timelineEvent(b.Resp, room, hasID(history)) != nil
	})
	if evt := timelineEvent(b.Resp, room, hasID(history)); evt.Type != event.EventEncrypted {
		t.Fatalf("the new device decrypted history without the backup: %s", evt.Type)
	}

	if err := s.VerifyWithRecoveryKey(e2e.Ctx(t), "EsTc 5rr1 4Jhp Uc18 hwCn 2b9T LSvj 5h4T TkP8 bdeK JGTa"); err == nil {
		t.Error("VerifyWithRecoveryKey accepted a wrong key")
	}
	if err := s.VerifyWithRecoveryKey(e2e.Ctx(t), recoveryKey); err != nil {
		t.Fatalf("VerifyWithRecoveryKey: %v", err)
	}
	ld := r.waitFor(t, "the history message, decrypted", func(ev Event) bool {
		ld, ok := ev.(LateDecrypted)
		return ok && ld.Room == room && len(ld.Events) > 0
	}).(LateDecrypted)
	if ld.Events[0].ID != history || ld.Events[0].Content.AsMessage().Body != "from before the second device" {
		t.Errorf("LateDecrypted = %v", ld.Events)
	}

	// The device is cross-signed now: the server has its self-signing
	// key's signature on the device keys.
	ctx := e2e.Ctx(t)
	mach := s.crypto.Machine
	pub, err := mach.GetOwnCrossSigningPublicKeys(ctx)
	if pub == nil {
		t.Fatalf("no cross-signing keys cached: %v", err)
	}
	resp, err := s.Client().QueryKeys(ctx, &mautrix.ReqQueryKeys{
		DeviceKeys: map[id.UserID]mautrix.DeviceIDList{aliceID: {creds.DeviceID}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sigs := resp.DeviceKeys[aliceID][creds.DeviceID].Signatures[aliceID]
	if _, ok := sigs[id.NewKeyID(id.KeyAlgorithmEd25519, pub.SelfSigningKey.String())]; !ok {
		t.Errorf("device keys are not signed by the self-signing key %s: %v", pub.SelfSigningKey, sigs)
	}
}

// TestRevokedTokenIsFatal: a sync rejected with M_UNKNOWN_TOKEN ends Run
// with a LoggedOutError, reported as Disconnected first.
func TestRevokedTokenIsFatal(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	creds, _ := login(t, srv, aliceID, password)
	s := newSession(t, creds, openStore(t, filepath.Join(t.TempDir(), "matrix.db")))
	if _, err := s.Client().Logout(e2e.Ctx(t)); err != nil {
		t.Fatalf("Logout: %v", err)
	}
	r := start(t, s)
	ev := r.waitFor(t, "Disconnected", func(ev Event) bool {
		c, ok := ev.(Connection)
		return ok && c.State == Disconnected
	}).(Connection)
	var lo *LoggedOutError
	if !errors.As(ev.Err, &lo) || lo.SoftLogout || !errors.Is(ev.Err, mautrix.MUnknownToken) {
		t.Errorf("Disconnected.Err = %v, want a hard LoggedOutError", ev.Err)
	}
	select {
	case err := <-r.done:
		r.done <- err // for the cleanup's stop
		if !errors.As(err, &lo) {
			t.Errorf("Run = %v, want *LoggedOutError", err)
		}
	case <-e2e.Ctx(t).Done():
		t.Fatal("Run did not return after a revoked token")
	}
}
