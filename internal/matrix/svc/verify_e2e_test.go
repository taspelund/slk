//go:build matrixe2e

package svc

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/rs/zerolog"

	"github.com/gammons/slk/internal/matrix/e2e"
	"github.com/gammons/slk/internal/matrix/ident"
	"github.com/gammons/slk/internal/matrix/rooms"
	"github.com/gammons/slk/internal/matrix/session"
	"github.com/gammons/slk/internal/matrix/store"
	"github.com/gammons/slk/internal/ui"
)

// TestVerifyThroughBackend: a read-only backend shows another device's
// request and the emoji as ui messages, the answers go through its
// VerificationService, and the outcome arrives in order: verified, then
// the keys.
func TestVerifyThroughBackend(t *testing.T) {
	srv := e2e.Homeserver(t)
	aliceID, password := e2e.RegisterUser(t, srv, "alice")
	peer := e2e.LoginPeer(t, srv, aliceID, password)
	peer.SetupRecovery(t)
	ctx := e2e.Ctx(t)

	creds, err := session.Login(ctx, srv.URL, aliceID.String(), password, "slk (verify e2e)")
	if err != nil {
		t.Fatalf("Login: %v", err)
	}
	st, err := store.Open(ctx, filepath.Join(t.TempDir(), "matrix.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	s, err := session.New(ctx, creds, st, zerolog.Nop())
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	ids := ident.New(st)
	m, err := rooms.New(ctx, st, ids, aliceID)
	if err != nil {
		t.Fatalf("rooms.New: %v", err)
	}
	b := New(Deps{Account: "e2e", ReadOnly: true, Session: s, Store: st, IDs: ids, Rooms: m})
	var mu sync.Mutex
	var sent []tea.Msg
	b.SetUISend(func(msg tea.Msg) {
		mu.Lock()
		sent = append(sent, msg)
		mu.Unlock()
	})
	b.Install(ui.NewApp())
	port := verifyPort{s}

	runCtx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		if err := s.Run(runCtx); err != nil && !errors.Is(err, context.Canceled) {
			t.Errorf("session Run: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		for range s.Events() {
		}
	}()
	t.Cleanup(func() { cancel(); wg.Wait() })

	index := func(match func(tea.Msg) bool) int {
		mu.Lock()
		defer mu.Unlock()
		return slices.IndexFunc(sent, match)
	}
	wait := func(what string, match func(tea.Msg) bool) tea.Msg {
		t.Helper()
		e2e.Eventually(t, what, func() bool { return index(match) >= 0 })
		mu.Lock()
		defer mu.Unlock()
		return sent[slices.IndexFunc(sent, match)]
	}

	txn := peer.RequestVerification(t)
	req := wait("the request", func(msg tea.Msg) bool {
		_, ok := msg.(ui.VerifyRequestMsg)
		return ok
	}).(ui.VerifyRequestMsg)
	if want := "e2e peer (" + peer.Client.DeviceID.String() + ")"; req.ID != string(txn) || req.Device != want {
		t.Fatalf("request = %+v, want ID %s from %q", req, txn, want)
	}
	if msg := port.Accept(req.ID); msg != nil {
		t.Fatalf("Accept = %v", msg)
	}
	emoji := wait("the emoji", func(msg tea.Msg) bool {
		_, ok := msg.(ui.VerifyEmojiMsg)
		return ok
	}).(ui.VerifyEmojiMsg)
	if emoji.ID != req.ID || len(emoji.Emoji) != 7 {
		t.Fatalf("emoji = %+v", emoji)
	}
	peer.WaitVerify(t, e2e.VerifySAS, txn)
	peer.ConfirmSAS(t, txn)
	if msg, ok := port.Confirm(req.ID).(ui.ToastMsg); !ok || msg.Text != "Waiting for the other device to confirm…" {
		t.Fatalf("Confirm = %v", msg)
	}
	isEnd := func(text string) func(tea.Msg) bool {
		return func(msg tea.Msg) bool {
			e, ok := msg.(ui.VerifyEndMsg)
			return ok && e.ID == req.ID && e.Text == text
		}
	}
	keys := isEnd("This device is verified; history restored from the key backup.")
	wait("the keys", keys)
	if done := index(isEnd("Verified with " + req.Device + "; getting the account's keys from it…")); done < 0 || done > index(keys) {
		t.Errorf("verified at %d, keys at %d: want verified first", done, index(keys))
	}
}
