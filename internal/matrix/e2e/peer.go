//go:build matrixe2e

package e2e

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/session/e2ee"
)

// Peer is the other side of a conversation: a logged-in device with
// end-to-end encryption (its own crypto database), syncing in the
// background from creation until the test ends. It records every room event
// it syncs, decrypted where it can, for WaitEvent.
type Peer struct {
	Client   *mautrix.Client
	Crypto   *e2ee.Helper
	UserID   id.UserID
	Password string

	mu     sync.Mutex
	seen   []*event.Event
	notify chan struct{} // closed and replaced when seen grows

	withheld map[id.RoomID]*crypto.OutboundGroupSession // by SendTextWithheld

	backupKey     *backup.MegolmBackupKey
	backupVersion id.KeyBackupVersion
}

// NewPeer registers a new user (prefix plus a random suffix) and logs a
// Peer device in for it.
func NewPeer(t testing.TB, srv Server, prefix string) *Peer {
	t.Helper()
	userID, password := RegisterUser(t, srv, prefix)
	return LoginPeer(t, srv, userID, password)
}

// LoginPeer logs a new Peer device in for an existing user.
func LoginPeer(t testing.TB, srv Server, userID id.UserID, password string) *Peer {
	t.Helper()
	ctx := Ctx(t)
	cli, err := mautrix.NewClient(srv.URL, "", "")
	if err != nil {
		t.Fatalf("e2e: client: %v", err)
	}
	_, err = cli.Login(ctx, &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: userID.String()},
		Password:                 password,
		InitialDeviceDisplayName: "e2e peer",
		StoreCredentials:         true,
	})
	if err != nil {
		t.Fatalf("e2e: login %s: %v", userID, err)
	}
	db := OpenDB(t, filepath.Join(t.TempDir(), "peer.db"))
	helper, err := e2ee.Setup(ctx, cli, db, []byte(randomHex(16)), zerolog.Nop())
	if err != nil {
		t.Fatalf("e2e: encryption for %s: %v", userID, err)
	}

	p := &Peer{Client: cli, Crypto: helper, UserID: userID, Password: password, notify: make(chan struct{})}
	syncCtx, cancel := context.WithCancel(context.Background())
	syncer := cli.Syncer.(*mautrix.DefaultSyncer)
	syncer.OnEvent(func(_ context.Context, evt *event.Event) {
		if evt.RoomID != "" && evt.StateKey == nil && evt.Type.Class == event.MessageEventType {
			p.record(evt)
		}
	})
	syncer.OnEventType(event.EventEncrypted, func(ctx context.Context, evt *event.Event) {
		// Record the decrypted form too, once the key is here.
		go p.decrypt(syncCtx, evt)
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		if err := cli.SyncWithContext(syncCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Logf("e2e: peer %s sync stopped: %v", userID, err)
		}
	}()
	// Registered after OpenDB's cleanup, so it runs first: stop syncing
	// before the database closes.
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return p
}

func (p *Peer) decrypt(ctx context.Context, evt *event.Event) {
	dec, err := p.Crypto.Decrypt(ctx, evt)
	if errors.Is(err, crypto.ErrNoSessionFound) {
		content := evt.Content.AsEncrypted()
		if p.Crypto.WaitForSession(ctx, evt.RoomID, content.SenderKey, content.SessionID, Timeout) {
			dec, err = p.Crypto.Decrypt(ctx, evt)
		}
	}
	if err == nil {
		p.record(dec)
	}
}

func (p *Peer) record(evt *event.Event) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, evt)
	close(p.notify)
	p.notify = make(chan struct{})
}

// WaitEvent waits until the peer has synced a room event in room that
// match accepts, and returns it. Encrypted events are offered to match
// twice: as received, and decrypted once the peer has the key.
func (p *Peer) WaitEvent(t testing.TB, room id.RoomID, match func(*event.Event) bool) *event.Event {
	t.Helper()
	ctx := Ctx(t)
	next := 0
	for {
		p.mu.Lock()
		seen, notify := p.seen, p.notify
		p.mu.Unlock()
		for ; next < len(seen); next++ {
			if evt := seen[next]; evt.RoomID == room && match(evt) {
				return evt
			}
		}
		select {
		case <-notify:
		case <-ctx.Done():
			t.Fatalf("e2e: %s: timed out waiting for an event in %s", p.UserID, room)
		}
	}
}

// WaitText waits for an m.room.message in room whose body is body, and
// returns it (decrypted, if the room is encrypted).
func (p *Peer) WaitText(t testing.TB, room id.RoomID, body string) *event.Event {
	t.Helper()
	return p.WaitEvent(t, room, func(evt *event.Event) bool {
		return evt.Type == event.EventMessage && evt.Content.AsMessage().Body == body
	})
}

// RoomOptions describe a room for CreateRoom.
type RoomOptions struct {
	Name      string
	Encrypted bool
	Invite    []id.UserID
	// Direct makes it a DM: is_direct on the invite, the
	// trusted_private_chat preset, and the room recorded under each invitee
	// in the creator's m.direct account data.
	Direct bool
}

// CreateRoom creates a room as the peer and returns its ID.
func (p *Peer) CreateRoom(t testing.TB, opts RoomOptions) id.RoomID {
	t.Helper()
	ctx := Ctx(t)
	req := &mautrix.ReqCreateRoom{
		Name:     opts.Name,
		Invite:   opts.Invite,
		Preset:   "private_chat",
		IsDirect: opts.Direct,
	}
	if opts.Direct {
		req.Preset = "trusted_private_chat"
	}
	if opts.Encrypted {
		req.InitialState = append(req.InitialState, &event.Event{
			Type:    event.StateEncryption,
			Content: event.Content{Parsed: &event.EncryptionEventContent{Algorithm: id.AlgorithmMegolmV1}},
		})
	}
	resp, err := p.Client.CreateRoom(ctx, req)
	if err != nil {
		t.Fatalf("e2e: %s: create room: %v", p.UserID, err)
	}
	if opts.Direct {
		direct := event.DirectChatsEventContent{}
		err := p.Client.GetAccountData(ctx, event.AccountDataDirectChats.Type, &direct)
		if err != nil && !errors.Is(err, mautrix.MNotFound) {
			t.Fatalf("e2e: %s: read m.direct: %v", p.UserID, err)
		}
		for _, u := range opts.Invite {
			direct[u] = append(direct[u], resp.RoomID)
		}
		if err := p.Client.SetAccountData(ctx, event.AccountDataDirectChats.Type, direct); err != nil {
			t.Fatalf("e2e: %s: write m.direct: %v", p.UserID, err)
		}
	}
	return resp.RoomID
}

// CreateDM creates a direct chat with other, encrypted or not.
func (p *Peer) CreateDM(t testing.TB, other id.UserID, encrypted bool) id.RoomID {
	t.Helper()
	return p.CreateRoom(t, RoomOptions{Encrypted: encrypted, Invite: []id.UserID{other}, Direct: true})
}

// Join joins room as the peer.
func (p *Peer) Join(t testing.TB, room id.RoomID) {
	t.Helper()
	if _, err := p.Client.JoinRoomByID(Ctx(t), room); err != nil {
		t.Fatalf("e2e: %s: join %s: %v", p.UserID, room, err)
	}
}

// WaitMember waits until the peer's state store has user in room with
// membership m (as synced).
func (p *Peer) WaitMember(t testing.TB, room id.RoomID, user id.UserID, m event.Membership) {
	t.Helper()
	Eventually(t, string(user)+" "+string(m)+" in "+string(room), func() bool {
		return p.Client.StateStore.IsMembership(context.Background(), room, user, m)
	})
}

// Send sends an m.room.message, encrypted if the room is, and returns its
// event ID.
func (p *Peer) Send(t testing.TB, room id.RoomID, content *event.MessageEventContent) id.EventID {
	t.Helper()
	resp, err := p.Client.SendMessageEvent(Ctx(t), room, event.EventMessage, content)
	if err != nil {
		t.Fatalf("e2e: %s: send to %s: %v", p.UserID, room, err)
	}
	return resp.EventID
}

// SendText sends a plain-text message.
func (p *Peer) SendText(t testing.TB, room id.RoomID, body string) id.EventID {
	t.Helper()
	return p.Send(t, room, &event.MessageEventContent{MsgType: event.MsgText, Body: body})
}

// SendHTML sends a message with an HTML formatted body and its plain-text
// fallback.
func (p *Peer) SendHTML(t testing.TB, room id.RoomID, body, html string) id.EventID {
	t.Helper()
	return p.Send(t, room, &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          body,
		Format:        event.FormatHTML,
		FormattedBody: html,
	})
}

// SendThreadReply sends body into the thread rooted at root (m.thread, with
// the root as the reply fallback).
func (p *Peer) SendThreadReply(t testing.TB, room id.RoomID, root id.EventID, body string) id.EventID {
	t.Helper()
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	content.RelatesTo = (&event.RelatesTo{}).SetThread(root, root)
	return p.Send(t, room, content)
}

// SendReply sends body as a reply (m.in_reply_to) to inReplyTo.
func (p *Peer) SendReply(t testing.TB, room id.RoomID, inReplyTo id.EventID, body string) id.EventID {
	t.Helper()
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	content.RelatesTo = (&event.RelatesTo{}).SetReplyTo(inReplyTo)
	return p.Send(t, room, content)
}

// SendEdit replaces target's body with body (m.replace).
func (p *Peer) SendEdit(t testing.TB, room id.RoomID, target id.EventID, body string) id.EventID {
	t.Helper()
	content := &event.MessageEventContent{MsgType: event.MsgText, Body: body}
	content.SetEdit(target) // body goes into m.new_content too
	content.Body = "* " + body
	return p.Send(t, room, content)
}

// React annotates target with key (an m.reaction; never encrypted).
func (p *Peer) React(t testing.TB, room id.RoomID, target id.EventID, key string) id.EventID {
	t.Helper()
	resp, err := p.Client.SendReaction(Ctx(t), room, target, key)
	if err != nil {
		t.Fatalf("e2e: %s: react in %s: %v", p.UserID, room, err)
	}
	return resp.EventID
}

// Redact redacts target.
func (p *Peer) Redact(t testing.TB, room id.RoomID, target id.EventID) id.EventID {
	t.Helper()
	resp, err := p.Client.RedactEvent(Ctx(t), room, target)
	if err != nil {
		t.Fatalf("e2e: %s: redact in %s: %v", p.UserID, room, err)
	}
	return resp.EventID
}

// ReadReceipt sends a public m.read receipt for evt.
func (p *Peer) ReadReceipt(t testing.TB, room id.RoomID, evt id.EventID) {
	t.Helper()
	if err := p.Client.SendReceipt(Ctx(t), room, evt, event.ReceiptTypeRead, nil); err != nil {
		t.Fatalf("e2e: %s: receipt in %s: %v", p.UserID, room, err)
	}
}

// SendTextWithheld sends an encrypted text message to room under a fresh
// Megolm session that the peer shares with nobody (other than its own
// devices), so its recipients cannot decrypt it until ShareRoomKey hands
// them the key.
func (p *Peer) SendTextWithheld(t testing.TB, room id.RoomID, body string) id.EventID {
	t.Helper()
	ctx := Ctx(t)
	mach := p.Crypto.Machine
	if err := mach.CryptoStore.RemoveOutboundGroupSession(ctx, room); err != nil {
		t.Fatalf("e2e: %s: drop outbound session: %v", p.UserID, err)
	}
	if err := mach.ShareGroupSession(ctx, room, []id.UserID{p.UserID}); err != nil {
		t.Fatalf("e2e: %s: new outbound session: %v", p.UserID, err)
	}
	// A copy of the session before it encrypts anything, so ShareRoomKey
	// can hand out the key from message index 0.
	unused, err := mach.CryptoStore.GetOutboundGroupSession(ctx, room)
	if err != nil || unused == nil {
		t.Fatalf("e2e: %s: read outbound session: %v", p.UserID, err)
	}
	encrypted, err := mach.EncryptMegolmEvent(ctx, room, event.EventMessage,
		&event.MessageEventContent{MsgType: event.MsgText, Body: body})
	if err != nil {
		t.Fatalf("e2e: %s: encrypt: %v", p.UserID, err)
	}
	resp, err := p.Client.SendMessageEvent(ctx, room, event.EventEncrypted, encrypted)
	if err != nil {
		t.Fatalf("e2e: %s: send to %s: %v", p.UserID, room, err)
	}
	p.mu.Lock()
	if p.withheld == nil {
		p.withheld = map[id.RoomID]*crypto.OutboundGroupSession{}
	}
	p.withheld[room] = unused
	p.mu.Unlock()
	return resp.EventID
}

// ShareRoomKey sends the key of the session SendTextWithheld last used in
// room to every device of users, as m.room_key to-device messages, the way
// a delayed key would arrive. The peer's next message to room starts a new
// session.
func (p *Peer) ShareRoomKey(t testing.TB, room id.RoomID, users ...id.UserID) {
	t.Helper()
	ctx := Ctx(t)
	p.mu.Lock()
	sess := p.withheld[room]
	delete(p.withheld, room)
	p.mu.Unlock()
	if sess == nil {
		t.Fatalf("e2e: %s: ShareRoomKey without SendTextWithheld in %s", p.UserID, room)
	}
	mach := p.Crypto.Machine
	// Put the index-0 copy back as the room's unshared outbound session,
	// so ShareGroupSession sends its key to users, then drop it: it must
	// not encrypt again.
	sess.Shared = false
	if err := mach.CryptoStore.AddOutboundGroupSession(ctx, sess); err != nil {
		t.Fatalf("e2e: %s: restore outbound session: %v", p.UserID, err)
	}
	if err := mach.ShareGroupSession(ctx, room, users); err != nil {
		t.Fatalf("e2e: %s: share room key: %v", p.UserID, err)
	}
	if err := mach.CryptoStore.RemoveOutboundGroupSession(ctx, room); err != nil {
		t.Fatalf("e2e: %s: drop outbound session: %v", p.UserID, err)
	}
}

// SetupRecovery makes the peer's account recoverable the way Element does
// on first login: cross-signing keys (stored in secret storage under a new
// default key), this device signed with them, and a server-side key backup
// whose private key is also in secret storage. It returns the recovery key.
// Call BackupRoomKeys to upload the room keys this device holds.
func (p *Peer) SetupRecovery(t testing.TB) string {
	t.Helper()
	ctx := Ctx(t)
	mach := p.Crypto.Machine
	recoveryKey, xsign, err := mach.GenerateAndUploadCrossSigningKeysWithPassword(ctx, p.Password, "")
	if err != nil {
		t.Fatalf("e2e: %s: cross-signing: %v", p.UserID, err)
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		t.Fatalf("e2e: %s: sign own device: %v", p.UserID, err)
	}
	if err := mach.SignOwnMasterKey(ctx); err != nil {
		t.Fatalf("e2e: %s: sign master key: %v", p.UserID, err)
	}

	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		t.Fatalf("e2e: %s: secret storage key: %v", p.UserID, err)
	}
	ssssKey, err := keyData.VerifyRecoveryKey(keyID, recoveryKey)
	if err != nil {
		t.Fatalf("e2e: %s: recovery key: %v", p.UserID, err)
	}

	bk, err := backup.NewMegolmBackupKey()
	if err != nil {
		t.Fatalf("e2e: backup key: %v", err)
	}
	// auth_data is signed without its signatures field.
	unsigned := struct {
		PublicKey id.Ed25519 `json:"public_key"`
	}{id.Ed25519(base64.RawStdEncoding.EncodeToString(bk.PublicKey().Bytes()))}
	deviceSig, err := mach.GetAccount().SignJSON(unsigned)
	if err != nil {
		t.Fatalf("e2e: sign backup with device: %v", err)
	}
	masterSig, err := xsign.MasterKey.SignJSON(unsigned)
	if err != nil {
		t.Fatalf("e2e: sign backup with master key: %v", err)
	}
	auth := backup.MegolmAuthData{
		PublicKey: unsigned.PublicKey,
		Signatures: map[id.UserID]map[id.KeyID]string{p.UserID: {
			id.NewKeyID(id.KeyAlgorithmEd25519, p.Client.DeviceID.String()):           deviceSig,
			id.NewKeyID(id.KeyAlgorithmEd25519, xsign.MasterKey.PublicKey().String()): masterSig,
		}},
	}
	resp, err := p.Client.CreateKeyBackupVersion(ctx, &mautrix.ReqRoomKeysVersionCreate[backup.MegolmAuthData]{
		Algorithm: id.KeyBackupAlgorithmMegolmBackupV1,
		AuthData:  auth,
	})
	if err != nil {
		t.Fatalf("e2e: %s: create key backup: %v", p.UserID, err)
	}
	if err := mach.SSSS.SetEncryptedAccountData(ctx, event.AccountDataMegolmBackupKey, bk.Bytes(), ssssKey); err != nil {
		t.Fatalf("e2e: %s: store backup key: %v", p.UserID, err)
	}
	p.backupKey, p.backupVersion = bk, resp.Version
	return recoveryKey
}

// BackupRoomKeys uploads every Megolm session the peer holds to the key
// backup SetupRecovery created, and returns how many it uploaded.
func (p *Peer) BackupRoomKeys(t testing.TB) int {
	t.Helper()
	if p.backupKey == nil {
		t.Fatalf("e2e: %s: BackupRoomKeys before SetupRecovery", p.UserID)
	}
	ctx := Ctx(t)
	sessions, err := p.Crypto.Machine.CryptoStore.GetAllGroupSessions(ctx).AsList()
	if err != nil {
		t.Fatalf("e2e: %s: list room keys: %v", p.UserID, err)
	}
	req := &mautrix.ReqKeyBackup{Rooms: map[id.RoomID]mautrix.ReqRoomKeyBackup{}}
	for _, igs := range sessions {
		first := igs.Internal.FirstKnownIndex()
		exported, err := igs.Internal.Export(first)
		if err != nil {
			t.Fatalf("e2e: export room key: %v", err)
		}
		enc, err := backup.EncryptSessionData(p.backupKey, backup.MegolmSessionData{
			Algorithm:          id.AlgorithmMegolmV1,
			ForwardingKeyChain: igs.ForwardingChains,
			SenderClaimedKeys:  backup.SenderClaimedKeys{Ed25519: igs.SigningKey},
			SenderKey:          igs.SenderKey,
			SessionKey:         string(exported),
		})
		if err != nil {
			t.Fatalf("e2e: encrypt room key for backup: %v", err)
		}
		raw, err := json.Marshal(enc)
		if err != nil {
			t.Fatalf("e2e: %v", err)
		}
		room := req.Rooms[igs.RoomID]
		if room.Sessions == nil {
			room.Sessions = map[id.SessionID]mautrix.ReqKeyBackupData{}
		}
		room.Sessions[igs.ID()] = mautrix.ReqKeyBackupData{
			FirstMessageIndex: int(first),
			ForwardedCount:    len(igs.ForwardingChains),
			SessionData:       raw,
		}
		req.Rooms[igs.RoomID] = room
	}
	if _, err := p.Client.PutKeysInBackup(ctx, p.backupVersion, req); err != nil {
		t.Fatalf("e2e: %s: upload room keys: %v", p.UserID, err)
	}
	return len(sessions)
}
