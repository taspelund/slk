//go:build matrixe2e

package e2e

import (
	"context"
	"testing"

	"maunium.net/go/mautrix/crypto/verificationhelper"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// The peer's side of interactive (SAS emoji) verification, on mautrix's
// verificationhelper, playing what Element does: it asks for or accepts
// a verification with another device of its user, shows the emoji, and
// answers the secret requests (m.secret.request) of a device it has
// verified, from the secrets SetupRecovery keeps.

// VerifyKind is what a VerifyEvent reports.
type VerifyKind int

const (
	VerifyRequested VerifyKind = iota
	VerifyReady
	VerifySAS
	VerifyDone
	VerifyCancelled
)

// VerifyEvent is one callback of the peer's verification helper.
type VerifyEvent struct {
	Kind   VerifyKind
	Txn    id.VerificationTransactionID
	Device id.DeviceID                  // VerifyRequested: the asking device; VerifyReady: the other
	Emoji  []string                     // VerifySAS: the emoji names
	Code   event.VerificationCancelCode // VerifyCancelled
}

// setupVerification installs the peer's verification helper (SAS only).
// LoginPeer calls it before the peer starts syncing.
func (p *Peer) setupVerification(ctx context.Context, t testing.TB) {
	t.Helper()
	p.verify = verificationhelper.NewVerificationHelper(p.Client, p.Crypto.Machine, nil, peerVerifyCallbacks{p}, false, false, true)
	if err := p.verify.Init(ctx); err != nil {
		t.Fatalf("e2e: %s: verification helper: %v", p.UserID, err)
	}
}

type peerVerifyCallbacks struct{ p *Peer }

func (c peerVerifyCallbacks) VerificationRequested(_ context.Context, txn id.VerificationTransactionID, _ id.UserID, dev id.DeviceID) {
	c.p.recordVerify(VerifyEvent{Kind: VerifyRequested, Txn: txn, Device: dev})
}

func (c peerVerifyCallbacks) VerificationReady(_ context.Context, txn id.VerificationTransactionID, dev id.DeviceID, _, _ bool, _ *verificationhelper.QRCode) {
	c.p.recordVerify(VerifyEvent{Kind: VerifyReady, Txn: txn, Device: dev})
}

func (c peerVerifyCallbacks) VerificationCancelled(_ context.Context, txn id.VerificationTransactionID, code event.VerificationCancelCode, _ string) {
	c.p.recordVerify(VerifyEvent{Kind: VerifyCancelled, Txn: txn, Code: code})
}

func (c peerVerifyCallbacks) VerificationDone(_ context.Context, txn id.VerificationTransactionID, _ event.VerificationMethod) {
	c.p.recordVerify(VerifyEvent{Kind: VerifyDone, Txn: txn})
}

func (c peerVerifyCallbacks) ShowSAS(_ context.Context, txn id.VerificationTransactionID, _ []rune, names []string, _ []int) {
	c.p.recordVerify(VerifyEvent{Kind: VerifySAS, Txn: txn, Emoji: names})
}

func (p *Peer) recordVerify(ev VerifyEvent) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.verifyEvents = append(p.verifyEvents, ev)
	close(p.notify)
	p.notify = make(chan struct{})
}

// WaitVerify waits for a verification callback of kind (for txn, unless it
// is empty) and returns it.
func (p *Peer) WaitVerify(t testing.TB, kind VerifyKind, txn id.VerificationTransactionID) VerifyEvent {
	t.Helper()
	ctx := Ctx(t)
	for {
		p.mu.Lock()
		evs, notify := p.verifyEvents, p.notify
		p.mu.Unlock()
		for _, ev := range evs {
			if ev.Kind == kind && (txn == "" || ev.Txn == txn) {
				return ev
			}
		}
		select {
		case <-notify:
		case <-ctx.Done():
			t.Fatalf("e2e: %s: timed out waiting for verification event %d (txn %q); got %+v", p.UserID, kind, txn, evs)
		}
	}
}

// RequestVerification asks the peer's user's other devices to verify the
// peer, after refreshing its list of them, and returns the transaction.
func (p *Peer) RequestVerification(t testing.TB) id.VerificationTransactionID {
	t.Helper()
	ctx := Ctx(t)
	if _, err := p.Crypto.Machine.FetchKeys(ctx, []id.UserID{p.UserID}, true); err != nil {
		t.Fatalf("e2e: %s: fetch own devices: %v", p.UserID, err)
	}
	txn, err := p.verify.StartVerification(ctx, p.UserID)
	if err != nil {
		t.Fatalf("e2e: %s: request verification: %v", p.UserID, err)
	}
	return txn
}

// AcceptVerification accepts the request txn. The peer leaves starting
// the emoji exchange to the other device.
func (p *Peer) AcceptVerification(t testing.TB, txn id.VerificationTransactionID) {
	t.Helper()
	if err := p.verify.AcceptVerification(Ctx(t), txn); err != nil {
		t.Fatalf("e2e: %s: accept verification: %v", p.UserID, err)
	}
}

// ConfirmSAS says the emoji of txn match.
func (p *Peer) ConfirmSAS(t testing.TB, txn id.VerificationTransactionID) {
	t.Helper()
	if err := p.verify.ConfirmSAS(Ctx(t), txn); err != nil {
		t.Fatalf("e2e: %s: confirm SAS: %v", p.UserID, err)
	}
}

// CancelVerification cancels txn with code.
func (p *Peer) CancelVerification(t testing.TB, txn id.VerificationTransactionID, code event.VerificationCancelCode) {
	t.Helper()
	if err := p.verify.CancelVerification(Ctx(t), txn, code, "cancelled by the e2e peer"); err != nil {
		t.Fatalf("e2e: %s: cancel verification: %v", p.UserID, err)
	}
}

// StartSAS starts the emoji exchange of txn from the peer's side, as
// Element may once a request is accepted, even when the other device
// starts it too.
func (p *Peer) StartSAS(t testing.TB, txn id.VerificationTransactionID) {
	t.Helper()
	if err := p.verify.StartSAS(Ctx(t), txn); err != nil {
		t.Fatalf("e2e: %s: start SAS: %v", p.UserID, err)
	}
}
