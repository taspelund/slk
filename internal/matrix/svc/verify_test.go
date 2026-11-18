package svc

import (
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/ui"
)

func TestVerifyMsg(t *testing.T) {
	dev := session.VerificationUpdate{ID: "t1", Device: "ABC", DeviceName: "Element"}
	with := func(f func(*session.VerificationUpdate)) session.VerificationUpdate {
		u := dev
		f(&u)
		return u
	}
	for _, tc := range []struct {
		name string
		u    session.VerificationUpdate
		want tea.Msg
	}{
		{"request", with(func(u *session.VerificationUpdate) { u.Step = session.VerificationRequested }),
			ui.VerifyRequestMsg{ID: "t1", Device: "Element (ABC)"}},
		{"request from an unnamed device", session.VerificationUpdate{ID: "t1", Device: "ABC"},
			ui.VerifyRequestMsg{ID: "t1", Device: "ABC"}},
		{"done", with(func(u *session.VerificationUpdate) { u.Step = session.VerificationDone }),
			ui.VerifyEndMsg{ID: "t1", Text: "Verified with Element (ABC); getting the account's keys from it…"}},
		{"all keys", with(func(u *session.VerificationUpdate) {
			u.Step, u.CrossSigning, u.Restored = session.VerificationSecrets, true, true
		}), ui.VerifyEndMsg{ID: "t1", Text: "This device is verified; history restored from the key backup."}},
		{"no backup", with(func(u *session.VerificationUpdate) {
			u.Step, u.CrossSigning, u.Err = session.VerificationSecrets, true, session.ErrNoKeyBackup
		}), ui.VerifyEndMsg{ID: "t1", Text: "This device is verified. The account has no key backup to restore history from."}},
		{"no backup key", with(func(u *session.VerificationUpdate) {
			u.Step, u.CrossSigning, u.Err = session.VerificationSecrets, true, fmt.Errorf("timeout")
		}), ui.VerifyEndMsg{ID: "t1", Text: "This device is verified, but the account's keys did not all arrive (timeout); slk --matrix-verify takes the recovery key."}},
		{"timed out", with(func(u *session.VerificationUpdate) {
			u.Step, u.TimedOut, u.Reason = session.VerificationCancelled, true, "verification timed out"
		}), ui.VerifyEndMsg{ID: "t1", Text: "Verification with Element (ABC) timed out"}},
		{"cancelled", with(func(u *session.VerificationUpdate) {
			u.Step, u.Reason = session.VerificationCancelled, "The user cancelled."
		}), ui.VerifyEndMsg{ID: "t1", Text: "Verification with Element (ABC) cancelled: The user cancelled."}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := verifyMsg(tc.u); fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Errorf("verifyMsg = %#v, want %#v", got, tc.want)
			}
		})
	}
	emoji := verifyMsg(session.VerificationUpdate{ID: "t1", Step: session.VerificationEmoji,
		Emoji: []session.SASEmoji{{Glyph: "🐶", Name: "Dog"}}}).(ui.VerifyEmojiMsg)
	if emoji.Device != "your other device" || !slices.Equal(emoji.Emoji, []ui.VerifyEmoji{{Glyph: "🐶", Name: "Dog"}}) {
		t.Errorf("emoji = %+v", emoji)
	}
}

// sendVerify delivers in the order it was called, though each send blocks.
func TestSendVerifyKeepsOrder(t *testing.T) {
	b := &Backend{profile: newProfileState(), write: newWriteState()}
	var mu sync.Mutex
	var got []tea.Msg
	b.SetUISend(func(m tea.Msg) {
		time.Sleep(time.Microsecond)
		mu.Lock()
		got = append(got, m)
		mu.Unlock()
	})
	var want []tea.Msg
	for i := range 50 {
		m := ui.VerifyEndMsg{ID: fmt.Sprint(i)}
		want = append(want, m)
		b.sendVerify(m)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == len(want) || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("got %v, want %v", got, want)
	}
}
