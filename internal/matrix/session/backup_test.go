package session

import (
	"encoding/json"
	"errors"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/id"
)

// TestBackupEntryRoundTrip: what backupEntry uploads is what mautrix's
// restore (DownloadAndStoreLatestKeyBackup) decrypts and imports back
// into the same session.
func TestBackupEntryRoundTrip(t *testing.T) {
	ogs, err := olm.NewOutboundGroupSession()
	if err != nil {
		t.Fatal(err)
	}
	const (
		own   = id.Curve25519("ownIdentityKey")
		other = id.Curve25519("otherIdentityKey")
		room  = id.RoomID("!room:example.org")
	)
	shared := true
	igs, err := crypto.NewInboundGroupSession(own, "signingKey", room, ogs.Key(), 0, 0, &shared, false)
	if err != nil {
		t.Fatal(err)
	}
	key, err := backup.NewMegolmBackupKey()
	if err != nil {
		t.Fatal(err)
	}

	entry, err := backupEntry(igs, key, own)
	if err != nil {
		t.Fatalf("backupEntry: %v", err)
	}
	if entry.FirstMessageIndex != 0 || entry.ForwardedCount != 0 || !entry.IsVerified {
		t.Errorf("entry = index %d, forwarded %d, verified %v; want 0, 0, true",
			entry.FirstMessageIndex, entry.ForwardedCount, entry.IsVerified)
	}
	var enc backup.EncryptedSessionData[backup.MegolmSessionData]
	if err := json.Unmarshal(entry.SessionData, &enc); err != nil {
		t.Fatalf("session_data: %v", err)
	}
	data, err := enc.Decrypt(key)
	if err != nil {
		t.Fatalf("Decrypt: %v", err)
	}
	if data.ForwardingKeyChain == nil {
		t.Error("forwarding_curve25519_key_chain is null, want an array")
	}
	var mach crypto.OlmMachine
	back, err := mach.ImportRoomKeyFromBackupWithoutSaving(t.Context(), "1", room, nil, igs.ID(), data)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if back.ID() != igs.ID() || back.SenderKey != own || back.SigningKey != "signingKey" ||
		back.SharedHistory == nil || !*back.SharedHistory {
		t.Errorf("imported %s sender %s signing %s shared %v, want %s %s signingKey true",
			back.ID(), back.SenderKey, back.SigningKey, back.SharedHistory, igs.ID(), own)
	}

	// Another device's session is not claimed verified.
	if entry, err := backupEntry(igs, key, other); err != nil || entry.IsVerified {
		t.Errorf("someone else's session: verified %v, err %v; want false", entry.IsVerified, err)
	}
}

// TestCheckBackupTrust: the session uploads only to a megolm v1 backup
// whose public key is its own backup key's.
func TestCheckBackupTrust(t *testing.T) {
	key, err := backup.NewMegolmBackupKey()
	if err != nil {
		t.Fatal(err)
	}
	otherKey, err := backup.NewMegolmBackupKey()
	if err != nil {
		t.Fatal(err)
	}
	info := func(alg id.KeyBackupAlgorithm, k *backup.MegolmBackupKey) *mautrix.RespRoomKeysVersion[backup.MegolmAuthData] {
		return &mautrix.RespRoomKeysVersion[backup.MegolmAuthData]{
			Algorithm: alg,
			AuthData:  backup.MegolmAuthData{PublicKey: backupPublicKey(k)},
			Version:   "3",
		}
	}
	if err := checkBackupTrust(key, info(id.KeyBackupAlgorithmMegolmBackupV1, key)); err != nil {
		t.Errorf("own backup: %v", err)
	}
	var skip backupSkip
	if err := checkBackupTrust(key, info(id.KeyBackupAlgorithmMegolmBackupV1, otherKey)); !errors.As(err, &skip) {
		t.Errorf("backup with another key: %v, want a backupSkip", err)
	}
	if err := checkBackupTrust(key, info("org.example.other", key)); !errors.As(err, &skip) {
		t.Errorf("backup with another algorithm: %v, want a backupSkip", err)
	}
}
