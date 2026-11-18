//go:build matrixe2e

package session

import (
	"path/filepath"
	"slices"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/signatures"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/e2e"
)

// verifyLog collects the updates a session's verification listener gets.
type verifyLog struct{ ch chan VerificationUpdate }

func listen(s *Session) *verifyLog {
	l := &verifyLog{ch: make(chan VerificationUpdate, 32)}
	s.SetVerificationListener(func(u VerificationUpdate) { l.ch <- u })
	return l
}

// next waits for the next update of step, failing on any cancel unless
// step is VerificationCancelled.
func (l *verifyLog) next(t *testing.T, step VerificationStep) VerificationUpdate {
	t.Helper()
	ctx := e2e.Ctx(t)
	for {
		select {
		case u := <-l.ch:
			if u.Step == step {
				return u
			}
			if u.Step == VerificationCancelled {
				t.Fatalf("verification cancelled while waiting for step %d: %+v", step, u)
			}
		case <-ctx.Done():
			t.Fatalf("timed out waiting for verification step %d", step)
		}
	}
}

func emojiNames(u VerificationUpdate) []string {
	var names []string
	for _, e := range u.Emoji {
		names = append(names, e.Name)
	}
	return names
}

// verifyFixture is an account with recovery set up on a peer device that
// stays online, a message from before slk's login backed up, and a running
// slk session that cannot read it yet.
type verifyFixture struct {
	peer    *e2e.Peer
	bob     *e2e.Peer
	room    id.RoomID
	history id.EventID
	creds   Credentials
	s       *Session
	r       *runner
	log     *verifyLog
}

func newVerifyFixture(t *testing.T) *verifyFixture {
	t.Helper()
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	peer := e2e.LoginPeer(t, srv, aliceID, password)
	peer.SetupRecovery(t)

	bob := e2e.NewPeer(t, srv, "bob")
	room := bob.CreateRoom(t, e2e.RoomOptions{Encrypted: true, Invite: []id.UserID{aliceID}})
	peer.Join(t, room)
	bob.WaitMember(t, room, aliceID, event.MembershipJoin)
	history := bob.SendText(t, room, "from before slk")
	if evt := peer.WaitText(t, room, "from before slk"); !evt.Mautrix.WasEncrypted {
		t.Fatal("the peer did not decrypt the history message")
	}
	if n := peer.BackupRoomKeys(t); n == 0 {
		t.Fatal("the peer backed up no room keys")
	}

	creds, _ := login(t, srv, aliceID, password)
	s := newSession(t, creds, openStore(t, filepath.Join(t.TempDir(), "matrix.db")))
	l := listen(s)
	r := start(t, s)
	b := r.waitBatch(t, "the history message", func(b SyncBatch) bool {
		return timelineEvent(b.Resp, room, hasID(history)) != nil
	})
	if evt := timelineEvent(b.Resp, room, hasID(history)); evt.Type != event.EventEncrypted {
		t.Fatalf("slk decrypted history before verifying: %s", evt.Type)
	}
	return &verifyFixture{peer: peer, bob: bob, room: room, history: history, creds: creds, s: s, r: r, log: l}
}

// crossSigned reports whether the server holds a valid signature of the
// account's self-signing key on slk's device keys: what makes every other
// device of the account (and other users who verified it) trust slk's
// device.
func (f *verifyFixture) crossSigned(t *testing.T) bool {
	t.Helper()
	ctx := e2e.Ctx(t)
	user := f.creds.UserID
	resp, err := f.peer.Client.QueryKeys(ctx, &mautrix.ReqQueryKeys{
		DeviceKeys: map[id.UserID]mautrix.DeviceIDList{user: {f.creds.DeviceID}},
	})
	if err != nil {
		t.Fatalf("query keys: %v", err)
	}
	ssk, ok := resp.SelfSigningKeys[user]
	if !ok {
		t.Fatal("the account has no self-signing key")
	}
	var sskKey id.Ed25519
	for _, k := range ssk.Keys {
		sskKey = k
	}
	dev, ok := resp.DeviceKeys[user][f.creds.DeviceID]
	if !ok {
		t.Fatal("slk's device keys are not on the server")
	}
	valid, err := signatures.VerifySignatureJSON(dev, user, sskKey.String(), sskKey)
	return err == nil && valid
}

// compareAndConfirm waits for both sides' emoji, checks they are the same
// seven, and confirms on both.
func (f *verifyFixture) compareAndConfirm(t *testing.T, txn string) {
	t.Helper()
	mine := f.log.next(t, VerificationEmoji)
	theirs := f.peer.WaitVerify(t, e2e.VerifySAS, id.VerificationTransactionID(txn))
	if len(mine.Emoji) != 7 || !slices.Equal(emojiNames(mine), theirs.Emoji) {
		t.Fatalf("emoji differ: slk %v, peer %v", emojiNames(mine), theirs.Emoji)
	}
	for _, e := range mine.Emoji {
		if e.Glyph == "" || e.Name == "" {
			t.Fatalf("emoji without glyph or name: %+v", mine.Emoji)
		}
	}
	f.peer.ConfirmSAS(t, id.VerificationTransactionID(txn))
	if err := f.s.ConfirmVerification(e2e.Ctx(t), txn); err != nil {
		t.Fatalf("ConfirmVerification: %v", err)
	}
}

// TestPeerRequestsVerification: another device of the account asks, slk
// accepts, both see the same emoji and confirm. slk's device is then
// cross-signed, and with the secrets the peer sends it restores a message
// from before its login and uploads its own new room keys to the backup.
func TestPeerRequestsVerification(t *testing.T) {
	f := newVerifyFixture(t)
	if f.crossSigned(t) {
		t.Fatal("slk's device is cross-signed before verifying")
	}

	txn := f.peer.RequestVerification(t)
	req := f.log.next(t, VerificationRequested)
	if req.ID != string(txn) || req.Device != f.peer.Client.DeviceID || req.DeviceName != "e2e peer" {
		t.Fatalf("request = %+v, want txn %s from %s (e2e peer)", req, txn, f.peer.Client.DeviceID)
	}
	if err := f.s.AcceptVerification(e2e.Ctx(t), req.ID); err != nil {
		t.Fatalf("AcceptVerification: %v", err)
	}
	f.compareAndConfirm(t, req.ID)
	f.peer.WaitVerify(t, e2e.VerifyDone, txn)
	if u := f.log.next(t, VerificationDone); u.Device != f.peer.Client.DeviceID {
		t.Errorf("done for device %s, want %s", u.Device, f.peer.Client.DeviceID)
	}
	sec := f.log.next(t, VerificationSecrets)
	if sec.Err != nil || !sec.Restored || !sec.CrossSigning {
		t.Fatalf("secrets: cross-signing=%v restored=%v err=%v", sec.CrossSigning, sec.Restored, sec.Err)
	}

	if !f.crossSigned(t) {
		t.Error("slk's device is not signed by the account's self-signing key")
	}
	ctx := e2e.Ctx(t)
	peerMach := f.peer.Crypto.Machine
	if dev, err := peerMach.GetOrFetchDevice(ctx, f.creds.UserID, f.creds.DeviceID); err != nil || !peerMach.IsDeviceTrusted(ctx, dev) {
		t.Errorf("the peer does not trust slk's device (err %v)", err)
	}
	if f.s.crypto.Machine.CrossSigningKeys == nil {
		t.Error("slk did not import the cross-signing keys")
	}

	// (c) The backup key is kept, history restored, new keys uploaded.
	if key, err := f.s.backupKey(ctx); err != nil || key == nil {
		t.Fatalf("no backup key kept: %v", err)
	}
	ld := f.r.waitFor(t, "the history message, decrypted", func(ev Event) bool {
		ld, ok := ev.(LateDecrypted)
		return ok && ld.Room == f.room && len(ld.Events) > 0
	}).(LateDecrypted)
	if ld.Events[0].ID != f.history || ld.Events[0].Content.AsMessage().Body != "from before slk" {
		t.Errorf("LateDecrypted = %v", ld.Events)
	}
	if _, err := f.s.Client().SendMessageEvent(ctx, f.room, event.EventMessage, &event.MessageEventContent{
		MsgType: event.MsgText, Body: "sent by verified slk",
	}); err != nil {
		t.Fatalf("send: %v", err)
	}
	f.bob.WaitText(t, f.room, "sent by verified slk")
	out, err := f.s.crypto.CryptoStore.GetOutboundGroupSession(ctx, f.room)
	if err != nil || out == nil {
		t.Fatalf("slk's outbound session: %v", err)
	}
	e2e.Eventually(t, "slk's own room key in the key backup", func() bool {
		version, err := f.s.Client().GetKeyBackupLatestVersion(ctx)
		if err != nil {
			return false
		}
		keys, err := f.s.Client().GetKeyBackupForRoom(ctx, version.Version, f.room)
		if err != nil {
			return false
		}
		_, ok := keys.Sessions[out.ID()]
		return ok
	})
}

// TestSessionRequestsVerification: slk asks, the peer accepts, both
// confirm; slk ends up cross-signed with the backup key.
func TestSessionRequestsVerification(t *testing.T) {
	f := newVerifyFixture(t)
	txn, err := f.s.RequestVerification(e2e.Ctx(t))
	if err != nil {
		t.Fatalf("RequestVerification: %v", err)
	}
	req := f.peer.WaitVerify(t, e2e.VerifyRequested, id.VerificationTransactionID(txn))
	if req.Device != f.creds.DeviceID {
		t.Fatalf("the peer got a request from %s, want %s", req.Device, f.creds.DeviceID)
	}
	f.peer.AcceptVerification(t, req.Txn)
	f.compareAndConfirm(t, txn)
	f.peer.WaitVerify(t, e2e.VerifyDone, req.Txn)
	f.log.next(t, VerificationDone)
	if sec := f.log.next(t, VerificationSecrets); sec.Err != nil || !sec.Restored || !sec.CrossSigning {
		t.Fatalf("secrets: cross-signing=%v restored=%v err=%v", sec.CrossSigning, sec.Restored, sec.Err)
	}
	if !f.crossSigned(t) {
		t.Error("slk's device is not signed by the account's self-signing key")
	}
	if key, err := f.s.backupKey(e2e.Ctx(t)); err != nil || key == nil {
		t.Errorf("no backup key kept: %v", err)
	}
}

// TestVerificationMismatchOrCancel: emoji slk says do not match, or a
// cancel from the peer, leave slk's device unverified, without the backup
// key.
func TestVerificationMismatchOrCancel(t *testing.T) {
	f := newVerifyFixture(t)

	// slk: they don't match.
	txn := f.peer.RequestVerification(t)
	req := f.log.next(t, VerificationRequested)
	if err := f.s.AcceptVerification(e2e.Ctx(t), req.ID); err != nil {
		t.Fatalf("AcceptVerification: %v", err)
	}
	f.log.next(t, VerificationEmoji)
	f.peer.WaitVerify(t, e2e.VerifySAS, txn)
	if err := f.s.CancelVerification(e2e.Ctx(t), req.ID, true); err != nil {
		t.Fatalf("CancelVerification: %v", err)
	}
	if ev := f.peer.WaitVerify(t, e2e.VerifyCancelled, txn); ev.Code != event.VerificationCancelCodeSASMismatch {
		t.Errorf("the peer got cancel code %s, want %s", ev.Code, event.VerificationCancelCodeSASMismatch)
	}

	// The peer cancels after the emoji.
	txn2 := f.peer.RequestVerification(t)
	req2 := f.log.next(t, VerificationRequested)
	if req2.ID != string(txn2) {
		t.Fatalf("second request %s, want %s", req2.ID, txn2)
	}
	if err := f.s.AcceptVerification(e2e.Ctx(t), req2.ID); err != nil {
		t.Fatalf("AcceptVerification: %v", err)
	}
	f.log.next(t, VerificationEmoji)
	f.peer.CancelVerification(t, txn2, event.VerificationCancelCodeUser)
	if u := f.log.next(t, VerificationCancelled); u.ID != req2.ID || u.ByUs || u.TimedOut {
		t.Errorf("cancel update = %+v", u)
	}

	if f.crossSigned(t) {
		t.Error("slk's device is cross-signed after a mismatch and a cancel")
	}
	if key, err := f.s.backupKey(e2e.Ctx(t)); err != nil || key != nil {
		t.Errorf("backup key kept after a mismatch and a cancel: %v (err %v)", key, err)
	}
	if f.s.crypto.Machine.CrossSigningKeys != nil {
		t.Error("cross-signing keys imported after a mismatch and a cancel")
	}
}

// TestBothStartSAS: when the peer starts the emoji exchange too, the
// spec's tie-break keeps one and the verification completes.
func TestBothStartSAS(t *testing.T) {
	f := newVerifyFixture(t)
	txn, err := f.s.RequestVerification(e2e.Ctx(t))
	if err != nil {
		t.Fatalf("RequestVerification: %v", err)
	}
	req := f.peer.WaitVerify(t, e2e.VerifyRequested, id.VerificationTransactionID(txn))
	f.peer.AcceptVerification(t, req.Txn)
	f.peer.StartSAS(t, req.Txn)
	f.compareAndConfirm(t, txn)
	f.peer.WaitVerify(t, e2e.VerifyDone, req.Txn)
	if sec := f.log.next(t, VerificationSecrets); sec.Err != nil || !sec.Restored || !sec.CrossSigning {
		t.Fatalf("secrets: cross-signing=%v restored=%v err=%v", sec.CrossSigning, sec.Restored, sec.Err)
	}
	if !f.crossSigned(t) {
		t.Error("slk's device is not signed by the account's self-signing key")
	}
}
