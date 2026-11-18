package session

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/id"
)

// Key backup upload: the session keeps the account's server-side key
// backup holding every Megolm session it has (the ones it creates to send
// with and the ones it receives), so a device signed in later can read
// what this one could, even when no other device is online to share keys.
//
// What counts as uploaded is mautrix's key_backup_version column on each
// stored inbound session: an upload marks the session with the backup
// version it went to, sessions restored from a backup already carry that
// backup's version, and a stored session that is replaced by a better copy
// (an earlier first index) is written with none. A pass uploads every
// session not marked with the server's current version, so when a new
// backup version is created elsewhere, everything is uploaded again.
//
// Only a backup this device trusts gets keys: one whose auth_data public
// key is the public half of the backup key the device holds. The device
// gets that key from secret storage in VerifyWithRecoveryKey and keeps it
// in the crypto store (encrypted with the pickle key), as the
// m.megolm_backup.v1 secret.

const (
	// backupDelay is how long after a new session arrives the uploader
	// waits, so sessions arriving together go up in one request.
	backupDelay = 2 * time.Second
	// backupInterval is how often the uploader checks anyway, to catch a
	// changed backup version and retry a failed upload.
	backupInterval = 10 * time.Minute
	// backupBatch is the most sessions in one upload request.
	backupBatch = 100
)

// backupSkip is why a pass uploaded nothing because the device has no
// backup it may upload to. It is logged once, not on every pass.
type backupSkip string

func (s backupSkip) Error() string { return string(s) }

// kickBackup asks the uploader for a pass soon. It never blocks.
func (s *Session) kickBackup() {
	select {
	case s.backupKick <- struct{}{}:
	default:
	}
}

// backupLoop runs upload passes until ctx is done: one at once, one
// backupDelay after each kick, and one every backupInterval.
func (s *Session) backupLoop(ctx context.Context) {
	tick := time.NewTicker(backupInterval)
	defer tick.Stop()
	for {
		s.backupPass(ctx)
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.backupKick:
			if !sleep(ctx, backupDelay) {
				return
			}
		}
		// This pass covers the kicks that came in while waiting.
		select {
		case <-s.backupKick:
		default:
		}
	}
}

// backupPass runs one upload pass and logs its outcome.
func (s *Session) backupPass(ctx context.Context) {
	n, err := s.uploadRoomKeys(ctx)
	var skip backupSkip
	switch {
	case errors.As(err, &skip):
		if string(skip) != s.backupSkipped {
			s.log.Info().Str("reason", string(skip)).Msg("Not uploading room keys to key backup")
			s.backupSkipped = string(skip)
		}
	case err != nil:
		if ctx.Err() == nil {
			s.log.Warn().Err(err).Int("uploaded", n).Msg("Failed to upload room keys to key backup")
		}
	default:
		s.backupSkipped = ""
		if n > 0 {
			s.log.Info().Int("count", n).Msg("Uploaded room keys to key backup")
		}
	}
}

// uploadRoomKeys uploads the stored sessions the current backup version
// lacks and returns how many it uploaded. It returns a backupSkip when
// there is no backup this device trusts.
func (s *Session) uploadRoomKeys(ctx context.Context) (int, error) {
	key, err := s.backupKey(ctx)
	if err != nil {
		return 0, err
	} else if key == nil {
		return 0, backupSkip("this device has no key backup key (verify it with the recovery key)")
	}
	info, err := s.client.GetKeyBackupLatestVersion(ctx)
	if errors.Is(err, mautrix.MNotFound) {
		return 0, backupSkip("the account has no key backup")
	} else if err != nil {
		return 0, fmt.Errorf("session: key backup version: %w", err)
	}
	if err := checkBackupTrust(key, info); err != nil {
		return 0, err
	}
	version := info.Version
	own := s.crypto.Machine.OwnIdentity().IdentityKey
	failed := map[id.SessionID]bool{}
	total := 0
	for {
		batch, err := s.sessionsToBackUp(ctx, version, failed)
		if err != nil {
			return total, err
		} else if len(batch) == 0 {
			return total, nil
		}
		req := &mautrix.ReqKeyBackup{Rooms: map[id.RoomID]mautrix.ReqRoomKeyBackup{}}
		var sent []*crypto.InboundGroupSession
		for _, igs := range batch {
			entry, err := backupEntry(igs, key, own)
			if err != nil {
				s.log.Warn().Err(err).Stringer("session_id", igs.ID()).Msg("Failed to prepare room key for key backup")
				failed[igs.ID()] = true
				continue
			}
			room := req.Rooms[igs.RoomID]
			if room.Sessions == nil {
				room.Sessions = map[id.SessionID]mautrix.ReqKeyBackupData{}
			}
			room.Sessions[igs.ID()] = entry
			req.Rooms[igs.RoomID] = room
			sent = append(sent, igs)
		}
		if len(sent) == 0 {
			continue
		}
		if _, err := s.client.PutKeysInBackup(ctx, version, req); err != nil {
			return total, fmt.Errorf("session: upload to key backup %s: %w", version, err)
		}
		for _, igs := range sent {
			if err := s.markBackedUp(ctx, igs, version); err != nil {
				return total, err
			}
		}
		total += len(sent)
	}
}

// sessionsToBackUp returns up to backupBatch stored sessions not marked
// with version, leaving out those in skip.
func (s *Session) sessionsToBackUp(ctx context.Context, version id.KeyBackupVersion, skip map[id.SessionID]bool) ([]*crypto.InboundGroupSession, error) {
	var batch []*crypto.InboundGroupSession
	err := s.crypto.CryptoStore.GetGroupSessionsWithoutKeyBackupVersion(ctx, version).Iter(func(igs *crypto.InboundGroupSession) (bool, error) {
		if !skip[igs.ID()] {
			batch = append(batch, igs)
		}
		return len(batch) < backupBatch, nil
	})
	if err != nil {
		return nil, fmt.Errorf("session: room keys to back up: %w", err)
	}
	return batch, nil
}

// markBackedUp marks igs as uploaded to version. If the store got a
// better copy of the session meanwhile (an earlier first index), the
// mark is taken back so the next pass uploads that copy: storing a
// session clears its mark, but a store between the read and this mark
// would otherwise be marked too.
func (s *Session) markBackedUp(ctx context.Context, igs *crypto.InboundGroupSession, version id.KeyBackupVersion) error {
	st := s.crypto.CryptoStore
	if err := st.SetGroupSessionKeyBackupVersion(ctx, igs.RoomID, igs.ID(), version); err != nil {
		return fmt.Errorf("session: mark room key backed up: %w", err)
	}
	now, err := st.GetGroupSession(ctx, igs.RoomID, igs.ID())
	if err != nil || now == nil || now.Internal.FirstKnownIndex() >= igs.Internal.FirstKnownIndex() {
		return nil
	}
	if err := st.SetGroupSessionKeyBackupVersion(ctx, igs.RoomID, igs.ID(), ""); err != nil {
		return fmt.Errorf("session: unmark room key: %w", err)
	}
	s.kickBackup()
	return nil
}

// backupEntry is igs encrypted for the backup whose key is key. own is
// this device's identity key: a session this device created is the only
// kind it marks verified.
func backupEntry(igs *crypto.InboundGroupSession, key *backup.MegolmBackupKey, own id.Curve25519) (mautrix.ReqKeyBackupData, error) {
	first := igs.Internal.FirstKnownIndex()
	exported, err := igs.Internal.Export(first)
	if err != nil {
		return mautrix.ReqKeyBackupData{}, fmt.Errorf("export room key: %w", err)
	}
	chain := igs.ForwardingChains
	if chain == nil {
		chain = []string{}
	}
	enc, err := backup.EncryptSessionData(key, backup.MegolmSessionData{
		Algorithm:          id.AlgorithmMegolmV1,
		ForwardingKeyChain: chain,
		SenderClaimedKeys:  backup.SenderClaimedKeys{Ed25519: igs.SigningKey},
		SenderKey:          igs.SenderKey,
		SessionKey:         string(exported),
		SharedHistory:      igs.SharedHistory,
	})
	if err != nil {
		return mautrix.ReqKeyBackupData{}, fmt.Errorf("encrypt room key: %w", err)
	}
	raw, err := json.Marshal(enc)
	if err != nil {
		return mautrix.ReqKeyBackupData{}, err
	}
	return mautrix.ReqKeyBackupData{
		FirstMessageIndex: int(first),
		ForwardedCount:    len(chain),
		IsVerified:        igs.SenderKey == own && len(chain) == 0 && igs.KeySource == id.KeySourceDirect,
		SessionData:       raw,
	}, nil
}

// checkBackupTrust returns a backupSkip unless info is a backup this
// device may upload to: a megolm v1 backup whose public key is key's.
func checkBackupTrust(key *backup.MegolmBackupKey, info *mautrix.RespRoomKeysVersion[backup.MegolmAuthData]) error {
	if info.Algorithm != id.KeyBackupAlgorithmMegolmBackupV1 {
		return backupSkip(fmt.Sprintf("key backup %s uses unsupported algorithm %s", info.Version, info.Algorithm))
	}
	if info.AuthData.PublicKey != backupPublicKey(key) {
		return backupSkip(fmt.Sprintf("key backup %s is not the one this device's backup key opens (verify it with the recovery key again)", info.Version))
	}
	return nil
}

func backupPublicKey(key *backup.MegolmBackupKey) id.Ed25519 {
	return id.Ed25519(base64.RawStdEncoding.EncodeToString(key.PublicKey().Bytes()))
}

// backupKey is the backup key this device keeps, or nil if it has none.
func (s *Session) backupKey(ctx context.Context) (*backup.MegolmBackupKey, error) {
	secret, err := s.crypto.CryptoStore.GetSecret(ctx, id.SecretMegolmBackupV1)
	if err != nil {
		return nil, fmt.Errorf("session: read key backup key: %w", err)
	} else if secret == "" {
		return nil, nil
	}
	raw, err := base64.RawStdEncoding.DecodeString(strings.TrimRight(secret, "="))
	if err != nil {
		return nil, fmt.Errorf("session: decode key backup key: %w", err)
	}
	key, err := backup.MegolmBackupKeyFromBytes(raw)
	if err != nil {
		return nil, fmt.Errorf("session: key backup key: %w", err)
	}
	return key, nil
}

// saveBackupKey keeps key as the device's trusted backup key.
func (s *Session) saveBackupKey(ctx context.Context, key *backup.MegolmBackupKey) error {
	err := s.crypto.CryptoStore.PutSecret(ctx, id.SecretMegolmBackupV1, base64.RawStdEncoding.EncodeToString(key.Bytes()))
	if err != nil {
		return fmt.Errorf("session: save key backup key: %w", err)
	}
	return nil
}
