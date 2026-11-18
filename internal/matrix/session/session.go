// Package session is a Matrix login: credentials on disk, the mautrix
// client, end-to-end encryption (pure-Go Olm/Megolm, stored in the
// account's store.Store), and the /sync loop. It delivers what it syncs as
// Events; it does not interpret rooms or talk to the UI.
//
// Logging: New logs through the zerolog.Logger it is given and nothing
// else; it never writes to stdout or stderr itself, because the TUI owns
// the terminal. slk passes DebugLogger, which writes into slk's debug log
// (internal/debuglog, enabled by SLK_DEBUG) and discards everything
// otherwise.
package session

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/ssss"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/session/e2ee"
	"github.com/gammons/slk/internal/matrix/store"
)

// Credentials are a logged-in device. They are saved as session.json, mode
// 0600, in the account directory ($XDG_DATA_HOME/slk/matrix/<account>).
type Credentials struct {
	Homeserver  string      `json:"homeserver"`
	UserID      id.UserID   `json:"user_id"`
	DeviceID    id.DeviceID `json:"device_id"`
	AccessToken string      `json:"access_token"`
	// PickleKey encrypts the Olm account and sessions at rest.
	PickleKey []byte `json:"pickle_key"`
}

// Login logs in with a password, creating a new device named deviceName,
// and returns its credentials with a fresh random pickle key. homeserver is
// the client-server API base URL (resolve a server name first, e.g. with
// mautrix.DiscoverClientAPI); if the server's login response names a
// different base URL in its well_known, the credentials keep that one.
func Login(ctx context.Context, homeserver, user, password, deviceName string) (Credentials, error) {
	cli, err := mautrix.NewClient(homeserver, "", "")
	if err != nil {
		return Credentials{}, fmt.Errorf("session: homeserver %q: %w", homeserver, err)
	}
	resp, err := cli.Login(ctx, &mautrix.ReqLogin{
		Type:                     mautrix.AuthTypePassword,
		Identifier:               mautrix.UserIdentifier{Type: mautrix.IdentifierTypeUser, User: user},
		Password:                 password,
		InitialDeviceDisplayName: deviceName,
	})
	if err != nil {
		return Credentials{}, fmt.Errorf("session: log in: %w", err)
	}
	if resp.WellKnown != nil && resp.WellKnown.Homeserver.BaseURL != "" {
		homeserver = strings.TrimRight(resp.WellKnown.Homeserver.BaseURL, "/")
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return Credentials{}, fmt.Errorf("session: pickle key: %w", err)
	}
	return Credentials{
		Homeserver:  homeserver,
		UserID:      resp.UserID,
		DeviceID:    resp.DeviceID,
		AccessToken: resp.AccessToken,
		PickleKey:   key,
	}, nil
}

// LoadCredentials reads session.json from dir. It refuses a file that
// anyone but its owner can read or write (except on Windows, which has no
// such mode bits).
func LoadCredentials(dir string) (Credentials, error) {
	return loadCredentials(dir)
}

// SaveCredentials writes session.json to dir with mode 0600, creating dir
// (and its parents) with mode 0700 and tightening dir to 0700 if it exists.
// It replaces any previous file atomically.
func SaveCredentials(dir string, c Credentials) error {
	return saveCredentials(dir, c)
}

// ConnState is the sync loop's connection state.
type ConnState int

const (
	Connecting ConnState = iota
	Connected
	Disconnected
)

// Event is something the session delivers. The concrete types are below.
type Event interface{ sessionEvent() }

// SyncBatch is one /sync response. Encrypted timeline events have been
// replaced by their decrypted form where the keys were available; the rest
// stay m.room.encrypted until a LateDecrypted delivers them. Initial is
// true for the response of the first sync since Run started.
//
// Decryption covers the timelines of joined and left rooms. A decrypted
// event has Mautrix.WasEncrypted set and event.SourceDecrypted in
// Mautrix.EventSource.
type SyncBatch struct {
	Resp    *mautrix.RespSync
	Initial bool
}

// LateDecrypted carries events of room that failed to decrypt when first
// delivered and have now been decrypted (keys arrived later, or were
// restored from key backup).
//
// It is always delivered after the SyncBatch that carried the events
// encrypted, with the events in timestamp order. Only events delivered by
// this Run are retried: an event that is still encrypted when slk exits is
// the consumer's to retry (with Client().Crypto.Decrypt) after a restart.
type LateDecrypted struct {
	Room   id.RoomID
	Events []*event.Event
}

// Connection reports a change in the sync loop's connection state. Err is
// the error that caused Disconnected, if any.
//
// Run starts with Connecting, reports Connected after the first successful
// sync, and after each failed one Disconnected (with the error) followed,
// once the backoff has passed, by Connecting. A fatal error is reported as
// Disconnected before Run returns it.
type Connection struct {
	State ConnState
	Err   error
}

func (SyncBatch) sessionEvent()     {}
func (LateDecrypted) sessionEvent() {}
func (Connection) sessionEvent()    {}

// Session is a running login.
type Session struct {
	client *mautrix.Client
	crypto *e2ee.Helper
	log    zerolog.Logger
	events chan Event
	tokens tokenStore
	keys   *pendingKeys
	ran    atomic.Bool
}

// New sets up the client and encryption for creds on st. It does not start
// syncing.
//
// It migrates mautrix's crypto and state store tables on st.DB, loads the
// device's Olm account (creating and uploading it the first time), and
// fails if st.DB already belongs to another device. The client sends
// encrypted to encrypted rooms on its own (Client().Crypto).
func New(ctx context.Context, creds Credentials, st *store.Store, log zerolog.Logger) (*Session, error) {
	if st == nil || st.DB == nil {
		return nil, errors.New("session: no store")
	}
	cli, err := mautrix.NewClient(creds.Homeserver, creds.UserID, creds.AccessToken)
	if err != nil {
		return nil, fmt.Errorf("session: client: %w", err)
	}
	cli.DeviceID = creds.DeviceID
	cli.Log = log
	syncer := mautrix.NewDefaultSyncer()
	syncer.FilterJSON = syncFilter()
	cli.Syncer = syncer

	helper, err := e2ee.Setup(ctx, cli, st.DB, creds.PickleKey, log)
	if err != nil {
		return nil, fmt.Errorf("session: %w", err)
	}
	s := &Session{
		client: cli,
		crypto: helper,
		log:    log.With().Str("component", "session").Logger(),
		events: make(chan Event),
		tokens: storeTokens{st: st, user: creds.UserID},
		keys:   newPendingKeys(),
	}
	helper.Machine.SessionReceived = func(_ context.Context, _ id.RoomID, session id.SessionID, _ uint32) {
		s.keys.received(session)
	}
	return s, nil
}

// Client is the underlying mautrix client, for requests other packages
// make (history, sending, media).
func (s *Session) Client() *mautrix.Client {
	return s.client
}

// Events delivers the session's events in order. It is closed when Run
// returns.
//
// It is unbuffered and Run never drops an event: Run blocks until the
// consumer takes each one, so a slow consumer slows the sync loop down (and
// with it the server's view of this device) rather than losing anything.
// The sync token is saved after the consumer has taken a SyncBatch, so a
// crash after that loses the batch unless the consumer has persisted it.
func (s *Session) Events() <-chan Event {
	return s.events
}

// Run syncs until ctx is cancelled, reconnecting with backoff, and returns
// ctx's error or a fatal one (such as a revoked token).
//
// Run may be called once per Session. It resumes from the sync token the
// last Run saved in the store (a full initial sync the first time). Each
// failed request is retried after 1s, 2s, 4s, … up to 30s (or longer, if
// the server asks to be left alone for longer). A revoked or missing access
// token (M_UNKNOWN_TOKEN, M_MISSING_TOKEN) is fatal and is returned as a
// *LoggedOutError.
func (s *Session) Run(ctx context.Context) error {
	if !s.ran.CompareAndSwap(false, true) {
		return errors.New("session: Run called twice")
	}
	defer close(s.events)
	return s.run(ctx)
}

// ErrNoKeyBackup is returned by VerifyWithRecoveryKey when the device was
// verified but the account has no server-side key backup (or no backup key
// in secret storage) to restore room keys from.
var ErrNoKeyBackup = errors.New("session: the account has no key backup")

// VerifyWithRecoveryKey verifies this device with the account's recovery
// key, then restores room keys from the server-side key backup.
//
// Verifying fetches the cross-signing keys from secret storage (SSSS) and
// signs this device and the master key with them. Restoring reads the
// backup's private key from secret storage and imports every room key in
// the latest backup version. While Run is running, events it delivered
// encrypted that the restored keys open are then delivered as
// LateDecrypted.
func (s *Session) VerifyWithRecoveryKey(ctx context.Context, key string) error {
	mach := s.crypto.Machine
	if err := mach.VerifyWithRecoveryKey(ctx, key); err != nil {
		return fmt.Errorf("session: verify with recovery key: %w", err)
	}
	keyID, keyData, err := mach.SSSS.GetDefaultKeyData(ctx)
	if err != nil {
		return fmt.Errorf("session: secret storage key: %w", err)
	}
	ssssKey, err := keyData.VerifyRecoveryKey(keyID, key)
	if err != nil && !errors.Is(err, ssss.ErrUnverifiableKey) {
		return fmt.Errorf("session: recovery key: %w", err)
	}
	raw, err := mach.SSSS.GetDecryptedAccountData(ctx, event.AccountDataMegolmBackupKey, ssssKey)
	if errors.Is(err, mautrix.MNotFound) {
		return ErrNoKeyBackup
	} else if err != nil {
		return fmt.Errorf("session: key backup key from secret storage: %w", err)
	}
	backupKey, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return fmt.Errorf("session: key backup key: %w", err)
	}
	version, err := mach.DownloadAndStoreLatestKeyBackup(ctx, backupKey)
	if errors.Is(err, mautrix.MNotFound) || (err == nil && version == "") {
		return ErrNoKeyBackup
	} else if err != nil {
		return fmt.Errorf("session: restore key backup: %w", err)
	}
	s.log.Info().Stringer("version", version).Msg("Restored room keys from key backup")
	return nil
}
