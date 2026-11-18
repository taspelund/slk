package session

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/crypto/backup"
	"maunium.net/go/mautrix/crypto/olm"
	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Interactive verification: this device and another of the account's
// devices (Element, iamb, another slk) verify each other by comparing
// seven emoji (SAS, the m.sas.v1 method; no QR codes), on mautrix's
// verificationhelper. Either side may ask: the other device sends a
// request that the user accepts here (VerificationRequested, then
// AcceptVerification), or RequestVerification asks all the account's other
// devices and the first to accept takes part. Once a request is accepted,
// this device starts the emoji exchange itself; if the other device starts
// it too, the spec's tie-break keeps one. Requests from other users are
// ignored: they are dropped without an answer, and the asking client sees
// them time out.
//
// A verification the user leaves unanswered times out ten minutes after
// the request (the helper's limit), and reports VerificationCancelled with
// TimedOut set. Transactions live in memory: a restart forgets them.
//
// When both sides have confirmed the emoji, the helper has trusted the
// other device and signed this account's master key with this device's
// key; the other device, holding the self-signing key, signs this device.
// This device then asks its verified devices for the account's secrets
// with m.secret.request (receiveSecrets): the three cross-signing private
// keys, which it uses as VerifyWithRecoveryKey does (signing itself with
// the self-signing key), and the key backup's private key, which it keeps
// and restores history with exactly as VerifyWithRecoveryKey does
// (restoreKeyBackup), so backup.go's uploads start. A secret is only taken
// from a device this one trusts, and only when it matches the account's
// published key for it.

// VerificationStep is how far an interactive verification has got.
type VerificationStep int

const (
	// VerificationRequested: another device of the account asks to verify
	// this one. Answer with AcceptVerification or CancelVerification.
	VerificationRequested VerificationStep = iota
	// VerificationEmoji: compare Emoji with the other device. Answer with
	// ConfirmVerification when they match, CancelVerification otherwise.
	VerificationEmoji
	// VerificationDone: both sides confirmed. The devices trust each
	// other; this one now asks the other for the account's secrets.
	VerificationDone
	// VerificationSecrets: the secrets step ended. CrossSigning says the
	// cross-signing keys arrived, Restored that the key backup's key
	// arrived and history was restored with it; Err is what fell short
	// (nil when everything arrived; ErrNoKeyBackup when the account has
	// no backup).
	VerificationSecrets
	// VerificationCancelled: either side cancelled, or it timed out
	// (TimedOut). Reason is the cancel's reason, ByUs whether this device
	// sent it.
	VerificationCancelled
)

// SASEmoji is one of the seven emoji to compare, with its spec name (what
// Element prints under it).
type SASEmoji struct {
	Glyph string
	Name  string
}

// VerificationUpdate is a step of an interactive verification, delivered to
// the VerificationListener.
type VerificationUpdate struct {
	// ID is the verification's transaction ID, which the answer methods
	// take.
	ID   string
	Step VerificationStep
	// Device is the other device, DeviceName its display name (empty
	// when unknown). Empty for a request this device sent that no device
	// has accepted yet.
	Device     id.DeviceID
	DeviceName string
	// Emoji are the seven emoji (VerificationEmoji).
	Emoji []SASEmoji
	// Reason, TimedOut and ByUs describe a cancel (VerificationCancelled).
	Reason   string
	TimedOut bool
	ByUs     bool
	// CrossSigning, Restored and Err are the secrets step's outcome
	// (VerificationSecrets).
	CrossSigning bool
	Restored     bool
	Err          error
}

// ErrNoOtherDevices is RequestVerification's error when the account has no
// other device to verify with.
var ErrNoOtherDevices = errors.New("session: the account has no other device to verify with")

// secretTimeout bounds the wait for each secret after a verification.
const secretTimeout = time.Minute

// verifier is the session's verification state: the helper and what the
// session remembers per transaction.
type verifier struct {
	helper *verificationhelper.VerificationHelper

	mu       sync.Mutex
	listener func(VerificationUpdate)
	// incoming holds the requests other devices sent. The rest are the
	// ones this device sent, whose emoji exchange starts once another
	// device accepts.
	incoming map[id.VerificationTransactionID]bool
	// devices is each transaction's other device, once known.
	devices map[id.VerificationTransactionID]id.DeviceID
}

// setupVerification installs the verification helper on the session's
// client. New calls it: the helper registers its handlers on the syncer,
// which must not change once Run syncs.
func (s *Session) setupVerification(ctx context.Context) error {
	s.verify = &verifier{
		incoming: map[id.VerificationTransactionID]bool{},
		devices:  map[id.VerificationTransactionID]id.DeviceID{},
	}
	s.verify.helper = verificationhelper.NewVerificationHelper(s.client, s.crypto.Machine, nil, verifyCallbacks{s}, false, false, true)
	if err := s.verify.helper.Init(ctx); err != nil {
		return fmt.Errorf("session: verification: %w", err)
	}
	return nil
}

// SetVerificationListener sets the function interactive verifications
// report their steps to. It is called on the session's goroutines (the
// sync loop, the helper's timers, the answer methods' callers) and must
// not block. Until it is set, requests from other devices are left
// unanswered.
func (s *Session) SetVerificationListener(fn func(VerificationUpdate)) {
	s.verify.mu.Lock()
	s.verify.listener = fn
	s.verify.mu.Unlock()
}

// RequestVerification asks the account's other devices to verify this one
// and returns the verification's ID. Their user accepts on one of them;
// the emoji follow as a VerificationEmoji update.
func (s *Session) RequestVerification(ctx context.Context) (string, error) {
	ctx = s.log.WithContext(ctx)
	own := s.client.UserID
	devices, err := s.crypto.Machine.FetchKeys(ctx, []id.UserID{own}, true)
	if err != nil {
		return "", fmt.Errorf("session: fetch own devices: %w", err)
	}
	others := 0
	for dev := range devices[own] {
		if dev != s.client.DeviceID {
			others++
		}
	}
	if others == 0 {
		return "", ErrNoOtherDevices
	}
	txn, err := s.verify.helper.StartVerification(ctx, own)
	if err != nil {
		return "", fmt.Errorf("session: request verification: %w", err)
	}
	return string(txn), nil
}

// AcceptVerification accepts the request id (a VerificationRequested
// update) and starts the emoji exchange.
func (s *Session) AcceptVerification(ctx context.Context, txnID string) error {
	ctx = s.log.WithContext(ctx)
	txn := id.VerificationTransactionID(txnID)
	// The device list is fresh before the keys are compared.
	if _, err := s.crypto.Machine.FetchKeys(ctx, []id.UserID{s.client.UserID}, true); err != nil {
		s.log.Warn().Err(err).Msg("Failed to refresh own devices before verification")
	}
	if err := s.verify.helper.AcceptVerification(ctx, txn); err != nil {
		return fmt.Errorf("session: accept verification: %w", err)
	}
	s.startSAS(ctx, txn)
	return nil
}

// ConfirmVerification tells the other device that the emoji of id match.
func (s *Session) ConfirmVerification(ctx context.Context, txnID string) error {
	ctx = s.log.WithContext(ctx)
	if err := s.verify.helper.ConfirmSAS(ctx, id.VerificationTransactionID(txnID)); err != nil {
		return fmt.Errorf("session: confirm verification: %w", err)
	}
	return nil
}

// CancelVerification ends id: a declined request, emoji that do not match
// (mismatch), or the user giving up. It reports no update: the caller
// knows.
func (s *Session) CancelVerification(ctx context.Context, txnID string, mismatch bool) error {
	ctx = s.log.WithContext(ctx)
	code, reason := event.VerificationCancelCodeUser, "The user cancelled the verification."
	if mismatch {
		code, reason = event.VerificationCancelCodeSASMismatch, "The emoji did not match."
	}
	txn := id.VerificationTransactionID(txnID)
	s.verify.forget(txn)
	if err := s.verify.helper.CancelVerification(ctx, txn, code, reason); err != nil {
		return fmt.Errorf("session: cancel verification: %w", err)
	}
	return nil
}

// startSAS starts the emoji exchange of txn, unless the other device has
// started it already (then the helper refuses, and theirs goes on).
func (s *Session) startSAS(ctx context.Context, txn id.VerificationTransactionID) {
	if err := s.verify.helper.StartSAS(ctx, txn); err != nil {
		s.log.Debug().Err(err).Stringer("transaction_id", txn).Msg("Not starting SAS")
	}
}

// report hands u to the listener, filling in the other device's name
// (fetching the device's keys if this device has not seen it yet).
func (s *Session) report(ctx context.Context, u VerificationUpdate) {
	v := s.verify
	v.mu.Lock()
	fn := v.listener
	if u.Device == "" {
		u.Device = v.devices[id.VerificationTransactionID(u.ID)]
	}
	v.mu.Unlock()
	if fn == nil {
		return
	}
	if u.Device != "" {
		if dev, err := s.crypto.Machine.GetOrFetchDevice(ctx, s.client.UserID, u.Device); err == nil && dev != nil {
			u.DeviceName = dev.Name
		}
	}
	fn(u)
}

func (v *verifier) forget(txn id.VerificationTransactionID) {
	v.mu.Lock()
	delete(v.incoming, txn)
	delete(v.devices, txn)
	v.mu.Unlock()
}

// verifyCallbacks are the helper's callbacks. The helper calls most of them
// with its transaction lock held, so they never call back into it on the
// same goroutine.
type verifyCallbacks struct{ s *Session }

var (
	_ verificationhelper.RequiredCallbacks = verifyCallbacks{}
	_ verificationhelper.ShowSASCallbacks  = verifyCallbacks{}
)

func (c verifyCallbacks) VerificationRequested(ctx context.Context, txn id.VerificationTransactionID, from id.UserID, fromDevice id.DeviceID) {
	s := c.s
	s.verify.mu.Lock()
	listening := s.verify.listener != nil
	s.verify.mu.Unlock()
	if from != s.client.UserID || !listening {
		// Not answered: the asking client times out. Dismissing drops
		// the transaction without sending anything.
		s.log.Info().Stringer("from", from).Stringer("device", fromDevice).Msg("Ignoring verification request")
		if err := s.verify.helper.DismissVerification(ctx, txn); err != nil {
			s.log.Warn().Err(err).Msg("Failed to dismiss verification request")
		}
		return
	}
	s.verify.mu.Lock()
	s.verify.incoming[txn] = true
	s.verify.devices[txn] = fromDevice
	s.verify.mu.Unlock()
	s.report(ctx, VerificationUpdate{ID: string(txn), Step: VerificationRequested, Device: fromDevice})
}

func (c verifyCallbacks) VerificationReady(ctx context.Context, txn id.VerificationTransactionID, otherDevice id.DeviceID, supportsSAS, _ bool, _ *verificationhelper.QRCode) {
	s := c.s
	v := s.verify
	v.mu.Lock()
	v.devices[txn] = otherDevice
	incoming := v.incoming[txn]
	v.mu.Unlock()
	if incoming {
		// AcceptVerification starts the exchange once the helper is done.
		return
	}
	go func() {
		if !supportsSAS {
			s.report(ctx, VerificationUpdate{ID: string(txn), Step: VerificationCancelled, ByUs: true, Reason: "the other device cannot verify with emoji"})
			_ = s.CancelVerification(ctx, string(txn), false)
			return
		}
		// Waits for the helper's lock, held until it has saved the
		// ready state.
		s.startSAS(s.log.WithContext(ctx), txn)
	}()
}

func (c verifyCallbacks) ShowSAS(ctx context.Context, txn id.VerificationTransactionID, emojis []rune, names []string, _ []int) {
	u := VerificationUpdate{ID: string(txn), Step: VerificationEmoji}
	for i, r := range emojis {
		e := SASEmoji{Glyph: string(r)}
		if i < len(names) {
			e.Name = names[i]
		}
		u.Emoji = append(u.Emoji, e)
	}
	c.s.report(ctx, u)
}

func (c verifyCallbacks) VerificationCancelled(ctx context.Context, txn id.VerificationTransactionID, code event.VerificationCancelCode, reason string) {
	s := c.s
	s.report(ctx, VerificationUpdate{
		ID:       string(txn),
		Step:     VerificationCancelled,
		Reason:   reason,
		TimedOut: code == event.VerificationCancelCodeTimeout,
	})
	s.verify.forget(txn)
}

func (c verifyCallbacks) VerificationDone(ctx context.Context, txn id.VerificationTransactionID, _ event.VerificationMethod) {
	s := c.s
	s.report(ctx, VerificationUpdate{ID: string(txn), Step: VerificationDone})
	s.verify.mu.Lock()
	device := s.verify.devices[txn]
	s.verify.mu.Unlock()
	s.verify.forget(txn)
	// Off the sync goroutine: the secrets arrive through /sync.
	go func() {
		xs, restored, err := s.receiveSecrets(ctx)
		if err != nil {
			s.log.Warn().Err(err).Msg("Verified, but not every secret arrived")
		}
		s.report(ctx, VerificationUpdate{ID: string(txn), Step: VerificationSecrets, Device: device, CrossSigning: xs, Restored: restored, Err: err})
	}()
}

// receiveSecrets asks the account's verified devices for its cross-signing
// keys and its key backup key, and uses what arrives as
// VerifyWithRecoveryKey uses the same keys from secret storage.
// crossSigning says the cross-signing keys arrived, restored that the
// backup key arrived and history was restored with it.
func (s *Session) receiveSecrets(ctx context.Context) (crossSigning, restored bool, err error) {
	ctx = s.log.WithContext(ctx)
	mach := s.crypto.Machine
	// The other device's signature on this one, and its own, are on the
	// server now.
	if _, err := mach.FetchKeys(ctx, []id.UserID{s.client.UserID}, true); err != nil {
		s.log.Warn().Err(err).Msg("Failed to refresh own devices after verification")
	}

	var xsErr, backupErr error
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		xsErr = s.receiveCrossSigningKeys(ctx)
	}()
	go func() {
		defer wg.Done()
		backupErr = s.receiveBackupKey(ctx)
	}()
	wg.Wait()
	return xsErr == nil, backupErr == nil, errors.Join(xsErr, backupErr)
}

// receiveCrossSigningKeys requests the three cross-signing private keys,
// imports them and signs this device with the self-signing one, as
// VerifyWithRecoveryKey does.
func (s *Session) receiveCrossSigningKeys(ctx context.Context) error {
	mach := s.crypto.Machine
	pub, err := mach.GetOwnCrossSigningPublicKeys(ctx)
	if err != nil {
		return fmt.Errorf("session: cross-signing keys: %w", err)
	} else if pub == nil {
		return errors.New("session: the account has no cross-signing keys")
	}
	wants := []struct {
		name id.Secret
		pub  id.Ed25519
	}{
		{id.SecretXSMaster, pub.MasterKey},
		{id.SecretXSSelfSigning, pub.SelfSigningKey},
		{id.SecretXSUserSigning, pub.UserSigningKey},
	}
	seeds := make([][]byte, len(wants))
	errs := make([]error, len(wants))
	var wg sync.WaitGroup
	for i, w := range wants {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[i] = mach.GetOrRequestSecret(ctx, w.name, func(secret string) (bool, error) {
				seed, err := decodeSecret(secret)
				if err != nil {
					return false, nil
				}
				key, err := olm.NewPKSigningFromSeed(seed)
				if err != nil || key.PublicKey() != w.pub {
					s.log.Warn().Stringer("secret", w.name).Msg("Ignoring a cross-signing key that is not the account's")
					return false, nil
				}
				seeds[i] = seed
				return true, nil
			}, secretTimeout)
			if errs[i] != nil {
				errs[i] = fmt.Errorf("session: %s: %w", w.name, errs[i])
			}
		}()
	}
	wg.Wait()
	if err := errors.Join(errs...); err != nil {
		return err
	}
	if err := mach.ImportCrossSigningKeys(crypto.CrossSigningSeeds{
		MasterKey:      seeds[0],
		SelfSigningKey: seeds[1],
		UserSigningKey: seeds[2],
	}); err != nil {
		return fmt.Errorf("session: import cross-signing keys: %w", err)
	}
	if err := mach.SignOwnDevice(ctx, mach.OwnIdentity()); err != nil {
		return fmt.Errorf("session: sign own device: %w", err)
	}
	return nil
}

// receiveBackupKey requests the key backup's private key, accepts it only
// when it opens the account's current backup, and restores from the
// backup with it (restoreKeyBackup, as VerifyWithRecoveryKey does).
func (s *Session) receiveBackupKey(ctx context.Context) error {
	info, err := s.client.GetKeyBackupLatestVersion(ctx)
	if errors.Is(err, mautrix.MNotFound) {
		return ErrNoKeyBackup
	} else if err != nil {
		return fmt.Errorf("session: key backup version: %w", err)
	}
	var key *backup.MegolmBackupKey
	err = s.crypto.Machine.GetOrRequestSecret(ctx, id.SecretMegolmBackupV1, func(secret string) (bool, error) {
		raw, err := decodeSecret(secret)
		if err != nil {
			return false, nil
		}
		k, err := backup.MegolmBackupKeyFromBytes(raw)
		if err != nil || checkBackupTrust(k, info) != nil {
			s.log.Warn().Msg("Ignoring a key backup key that does not open the account's backup")
			return false, nil
		}
		key = k
		return true, nil
	}, secretTimeout)
	if err != nil {
		return fmt.Errorf("session: key backup key: %w", err)
	}
	return s.restoreKeyBackup(ctx, key)
}

// decodeSecret decodes a shared secret: base64, padded or not.
func decodeSecret(secret string) ([]byte, error) {
	return base64.RawStdEncoding.DecodeString(strings.TrimRight(secret, "="))
}
