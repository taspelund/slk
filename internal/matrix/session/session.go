// Package session is a Matrix login: credentials on disk, the mautrix
// client, end-to-end encryption (pure-Go Olm/Megolm, stored in the
// account's store.Store), and the /sync loop. It delivers what it syncs as
// Events; it does not interpret rooms or talk to the UI.
package session

import (
	"context"

	"github.com/rs/zerolog"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

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
// and returns its credentials with a fresh random pickle key.
func Login(ctx context.Context, homeserver, user, password, deviceName string) (Credentials, error) {
	panic("session: Login not implemented (WP-Session)")
}

// LoadCredentials reads session.json from dir.
func LoadCredentials(dir string) (Credentials, error) {
	panic("session: LoadCredentials not implemented (WP-Session)")
}

// SaveCredentials writes session.json to dir with mode 0600.
func SaveCredentials(dir string, c Credentials) error {
	panic("session: SaveCredentials not implemented (WP-Session)")
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
type SyncBatch struct {
	Resp    *mautrix.RespSync
	Initial bool
}

// LateDecrypted carries events of room that failed to decrypt when first
// delivered and have now been decrypted (keys arrived later, or were
// restored from key backup).
type LateDecrypted struct {
	Room   id.RoomID
	Events []*event.Event
}

// Connection reports a change in the sync loop's connection state. Err is
// the error that caused Disconnected, if any.
type Connection struct {
	State ConnState
	Err   error
}

func (SyncBatch) sessionEvent()     {}
func (LateDecrypted) sessionEvent() {}
func (Connection) sessionEvent()    {}

// Session is a running login.
type Session struct{}

// New sets up the client and encryption for creds on st. It does not start
// syncing.
func New(ctx context.Context, creds Credentials, st *store.Store, log zerolog.Logger) (*Session, error) {
	panic("session: New not implemented (WP-Session)")
}

// Client is the underlying mautrix client, for requests other packages
// make (history, sending, media).
func (s *Session) Client() *mautrix.Client {
	panic("session: Client not implemented (WP-Session)")
}

// Events delivers the session's events in order. It is closed when Run
// returns.
func (s *Session) Events() <-chan Event {
	panic("session: Events not implemented (WP-Session)")
}

// Run syncs until ctx is cancelled, reconnecting with backoff, and returns
// ctx's error or a fatal one (such as a revoked token).
func (s *Session) Run(ctx context.Context) error {
	panic("session: Run not implemented (WP-Session)")
}

// VerifyWithRecoveryKey verifies this device with the account's recovery
// key, then restores room keys from the server-side key backup.
func (s *Session) VerifyWithRecoveryKey(ctx context.Context, key string) error {
	panic("session: VerifyWithRecoveryKey not implemented (WP-Session)")
}
