package svc

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/ui"
)

// Interactive verification (WP-Verify): the session's verification steps
// go to the UI as ui.Verify*Msg values, and the UI's answers come back
// through core.VerificationService (verifyPort).
//
// Read-only mode keeps it, as it keeps verifying with the recovery key:
// verification changes nothing anyone else sees in a room. It changes
// only this device's own standing (signatures on its keys and the
// account's master key, to-device messages to the user's own devices),
// and every step waits for the user.

// verifyTimeout bounds each answer's requests.
const verifyTimeout = 30 * time.Second

// installVerification wires verification into app: the session reports
// to the backend, and the App answers through verifyPort.
func (b *Backend) installVerification(app *ui.App) {
	if b.d.Session == nil {
		return
	}
	b.d.Session.SetVerificationListener(b.onVerification)
	app.SetVerificationService(verifyPort{b.d.Session})
}

// verifyOut sends verification messages to the UI in the order the
// session reported them (uiSend gives each call its own goroutine, which
// could reorder a step and the next).
type verifyOut struct {
	mu      sync.Mutex
	queue   []tea.Msg
	sending bool
}

// sendVerify queues m for the UI. It never blocks: the session calls it
// with its verification lock held.
func (b *Backend) sendVerify(m tea.Msg) {
	o := &b.verifyOut
	o.mu.Lock()
	o.queue = append(o.queue, m)
	if o.sending {
		o.mu.Unlock()
		return
	}
	o.sending = true
	o.mu.Unlock()
	go func() {
		for {
			o.mu.Lock()
			if len(o.queue) == 0 {
				o.sending = false
				o.mu.Unlock()
				return
			}
			m := o.queue[0]
			o.queue = o.queue[1:]
			o.mu.Unlock()
			b.profile.mu.Lock()
			send := b.profile.send
			b.profile.mu.Unlock()
			if send != nil {
				send(m)
			}
		}
	}()
}

// onVerification is the session's verification listener.
func (b *Backend) onVerification(u session.VerificationUpdate) {
	if m := verifyMsg(u); m != nil {
		b.sendVerify(m)
	}
}

// verifyMsg is the UI message for u.
func verifyMsg(u session.VerificationUpdate) tea.Msg {
	dev := deviceLabel(u)
	switch u.Step {
	case session.VerificationRequested:
		return ui.VerifyRequestMsg{ID: u.ID, Device: dev}
	case session.VerificationEmoji:
		m := ui.VerifyEmojiMsg{ID: u.ID, Device: dev}
		for _, e := range u.Emoji {
			m.Emoji = append(m.Emoji, ui.VerifyEmoji{Glyph: e.Glyph, Name: e.Name})
		}
		return m
	case session.VerificationDone:
		return ui.VerifyEndMsg{ID: u.ID, Text: "Verified with " + dev + "; getting the account's keys from it…"}
	case session.VerificationSecrets:
		return ui.VerifyEndMsg{ID: u.ID, Text: secretsText(u)}
	case session.VerificationCancelled:
		text := "Verification with " + dev + " cancelled"
		if u.TimedOut {
			text = "Verification with " + dev + " timed out"
		} else if u.Reason != "" {
			text += ": " + u.Reason
		}
		return ui.VerifyEndMsg{ID: u.ID, Text: text}
	}
	return nil
}

// secretsText words the secrets step's outcome.
func secretsText(u session.VerificationUpdate) string {
	switch {
	case u.CrossSigning && u.Restored:
		return "This device is verified; history restored from the key backup."
	case u.CrossSigning && errors.Is(u.Err, session.ErrNoKeyBackup):
		return "This device is verified. The account has no key backup to restore history from."
	case u.Restored:
		return "This device is verified and history restored, but the cross-signing keys did not arrive."
	default:
		return fmt.Sprintf("This device is verified, but the account's keys did not all arrive (%v); slk --matrix-verify takes the recovery key.", u.Err)
	}
}

// deviceLabel names u's other device: "Name (ID)", or the ID alone.
func deviceLabel(u session.VerificationUpdate) string {
	switch {
	case u.Device == "":
		return "your other device"
	case u.DeviceName == "":
		return string(u.Device)
	default:
		return fmt.Sprintf("%s (%s)", u.DeviceName, u.Device)
	}
}

// verifyPort is core.VerificationService on the session.
type verifyPort struct{ s *session.Session }

var _ core.VerificationService = verifyPort{}

func (p verifyPort) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), verifyTimeout)
}

func (p verifyPort) Request() core.Msg {
	ctx, cancel := p.ctx()
	defer cancel()
	_, err := p.s.RequestVerification(ctx)
	switch {
	case errors.Is(err, session.ErrNoOtherDevices):
		return ui.ToastMsg{Text: "This account has no other device to verify with"}
	case err != nil:
		return ui.ToastMsg{Text: "Couldn't ask your other devices: " + err.Error()}
	}
	return ui.ToastMsg{Text: "Verification request sent: accept it on another of your devices"}
}

func (p verifyPort) Accept(id string) core.Msg {
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.s.AcceptVerification(ctx, id); err != nil {
		return ui.ToastMsg{Text: "Couldn't accept the verification: " + err.Error()}
	}
	return nil
}

func (p verifyPort) Confirm(id string) core.Msg {
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.s.ConfirmVerification(ctx, id); err != nil {
		return ui.ToastMsg{Text: "Couldn't confirm the verification: " + err.Error()}
	}
	return ui.ToastMsg{Text: "Waiting for the other device to confirm…"}
}

func (p verifyPort) Cancel(id string, mismatch bool) core.Msg {
	ctx, cancel := p.ctx()
	defer cancel()
	if err := p.s.CancelVerification(ctx, id, mismatch); err != nil {
		return ui.ToastMsg{Text: "Couldn't cancel the verification: " + err.Error()}
	}
	if mismatch {
		return ui.ToastMsg{Text: "Verification cancelled: the emoji did not match. This device stays unverified."}
	}
	return ui.ToastMsg{Text: "Verification cancelled"}
}
