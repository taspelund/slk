// Package e2ee sets up end-to-end encryption for a mautrix client: an
// OlmMachine on mautrix's SQL crypto store, mautrix's SQL state store, both
// on a database the caller owns, wired into the client's syncer. It is what
// mautrix's cryptohelper does, minus cryptohelper's import of
// go.mau.fi/util/dbutil/litestream, which links the cgo SQLite driver
// (github.com/mattn/go-sqlite3) into any cgo build; slk uses the pure-Go
// modernc driver only.
//
// session uses it for the account's own device; the matrixe2e test harness
// uses it for its peers.
package e2ee

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/rs/zerolog"
	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"
)

// Helper is a client's encryption. It implements mautrix.CryptoHelper, so
// the client encrypts what it sends to encrypted rooms.
type Helper struct {
	Client      *mautrix.Client
	Machine     *crypto.OlmMachine
	CryptoStore *crypto.SQLCryptoStore
	StateStore  *sqlstatestore.SQLStateStore
	log         zerolog.Logger
}

var _ mautrix.CryptoHelper = (*Helper)(nil)

// Setup migrates the crypto and state store tables on db, loads (or
// creates) the Olm account for cli's device, makes sure the device's keys
// are on the server, and installs the stores and the helper on cli:
// cli.StateStore, cli.Store (so the sync token lives in the crypto store),
// cli.Crypto. cli must be logged in (UserID, DeviceID, AccessToken) and its
// Syncer must be a mautrix.ExtensibleSyncer: Setup registers the crypto
// machine's and the state store's sync handlers on it. db is not closed by
// the helper.
//
// The pickle key encrypts the Olm account and sessions at rest; every
// Setup on the same db must use the same one.
func Setup(ctx context.Context, cli *mautrix.Client, db *dbutil.Database, pickleKey []byte, log zerolog.Logger) (*Helper, error) {
	if len(pickleKey) == 0 {
		return nil, errors.New("e2ee: pickle key must be provided")
	}
	if cli.UserID == "" || cli.DeviceID == "" || cli.AccessToken == "" {
		return nil, errors.New("e2ee: the client must be logged in")
	}
	syncer, ok := cli.Syncer.(mautrix.ExtensibleSyncer)
	if !ok {
		return nil, errors.New("e2ee: the client's syncer must implement mautrix.ExtensibleSyncer")
	}
	log = log.With().Str("component", "crypto").Logger()

	stateStore := sqlstatestore.NewSQLStateStore(db, dbutil.ZeroLogger(log.With().Str("db_section", "matrix_state").Logger()), false)
	if err := stateStore.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("e2ee: migrate state store: %w", err)
	}
	cryptoStore := crypto.NewSQLCryptoStore(db, dbutil.ZeroLogger(log.With().Str("db_section", "crypto").Logger()), "", cli.DeviceID, pickleKey)
	if err := cryptoStore.DB.Upgrade(ctx); err != nil {
		return nil, fmt.Errorf("e2ee: migrate crypto store: %w", err)
	}
	stored, err := cryptoStore.FindDeviceID(ctx)
	if err != nil {
		return nil, fmt.Errorf("e2ee: read stored device ID: %w", err)
	}
	if stored != "" && stored != cli.DeviceID {
		return nil, fmt.Errorf("e2ee: database belongs to device %s, not %s", stored, cli.DeviceID)
	}

	cli.StateStore = stateStore
	cli.Store = cryptoStore
	mach := crypto.NewOlmMachine(cli, &log, cryptoStore, stateStore)
	if err := mach.Load(ctx); err != nil {
		return nil, fmt.Errorf("e2ee: load olm account: %w", err)
	}
	h := &Helper{Client: cli, Machine: mach, CryptoStore: cryptoStore, StateStore: stateStore, log: log}
	if err := h.checkDeviceKeys(ctx); err != nil {
		return nil, err
	}

	syncer.OnSync(mach.ProcessSyncResponse)
	syncer.OnEventType(event.StateMember, mach.HandleMemberEvent)
	syncer.OnEventType(event.StateHistoryVisibility, mach.HandleHistoryVisibility)
	syncer.OnEvent(cli.StateStoreSyncHandler)
	cli.Crypto = h
	return h, nil
}

// checkDeviceKeys uploads the device's identity and one-time keys the
// first time, and afterwards checks that the server still has the same
// ones (as cryptohelper does).
func (h *Helper) checkDeviceKeys(ctx context.Context) error {
	cli := h.Client
	resp, err := cli.QueryKeys(ctx, &mautrix.ReqQueryKeys{
		DeviceKeys: map[id.UserID]mautrix.DeviceIDList{cli.UserID: {cli.DeviceID}},
	})
	if err != nil {
		return fmt.Errorf("e2ee: query own device keys: %w", err)
	}
	shared := h.Machine.GetAccount().Shared
	device, ok := resp.DeviceKeys[cli.UserID][cli.DeviceID]
	switch {
	case !ok || len(device.Keys) == 0:
		if shared {
			return errors.New("e2ee: olm account is marked as shared, but its keys are gone from the server")
		}
		if err := h.Machine.ShareKeys(ctx, -1); err != nil {
			return fmt.Errorf("e2ee: upload device keys: %w", err)
		}
		return nil
	case !shared:
		return errors.New("e2ee: the server has keys for this device, but the olm account was never shared")
	case device.Keys.GetEd25519(cli.DeviceID) != h.Machine.OwnIdentity().SigningKey:
		return errors.New("e2ee: the server has a different identity key for this device")
	}
	return nil
}

// Init does nothing: Setup has done it. It is here for mautrix.CryptoHelper.
func (h *Helper) Init(context.Context) error { return nil }

// Decrypt decrypts a Megolm-encrypted room event.
func (h *Helper) Decrypt(ctx context.Context, evt *event.Event) (*event.Event, error) {
	return h.Machine.DecryptMegolmEvent(ctx, evt)
}

// Encrypt encrypts content for room, sharing a new group session with the
// room's joined and invited members first when there is none (or it has
// expired).
func (h *Helper) Encrypt(ctx context.Context, room id.RoomID, evtType event.Type, content any) (*event.EncryptedEventContent, error) {
	encrypted, err := h.Machine.EncryptMegolmEvent(ctx, room, evtType, content)
	if err == nil {
		return encrypted, nil
	}
	if !errors.Is(err, crypto.ErrSessionExpired) && !errors.Is(err, crypto.ErrNoGroupSession) && !errors.Is(err, crypto.ErrSessionNotShared) {
		return nil, err
	}
	users, err := h.StateStore.GetRoomJoinedOrInvitedMembers(ctx, room)
	if err != nil {
		return nil, fmt.Errorf("e2ee: room members: %w", err)
	}
	if err := h.Machine.ShareGroupSession(ctx, room, users); err != nil {
		return nil, fmt.Errorf("e2ee: share group session: %w", err)
	}
	encrypted, err = h.Machine.EncryptMegolmEvent(ctx, room, evtType, content)
	if err != nil {
		return nil, fmt.Errorf("e2ee: encrypt after sharing group session: %w", err)
	}
	return encrypted, nil
}

// WaitForSession waits up to timeout for the given Megolm session to
// arrive.
func (h *Helper) WaitForSession(ctx context.Context, room id.RoomID, senderKey id.SenderKey, session id.SessionID, timeout time.Duration) bool {
	return h.Machine.WaitForSession(ctx, room, senderKey, session, timeout)
}

// RequestSession asks the sender's device (all of them when device is
// empty) and this user's other devices for a Megolm session.
func (h *Helper) RequestSession(ctx context.Context, room id.RoomID, senderKey id.SenderKey, session id.SessionID, sender id.UserID, device id.DeviceID) {
	if device == "" {
		device = "*"
	}
	err := h.Machine.SendRoomKeyRequest(ctx, room, senderKey, session, "", map[id.UserID][]id.DeviceID{
		sender:          {device},
		h.Client.UserID: {"*"},
	})
	if err != nil {
		h.log.Warn().Err(err).Stringer("session_id", session).Msg("Failed to send key request")
	}
}
