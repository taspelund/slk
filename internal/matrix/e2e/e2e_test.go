//go:build matrixe2e

package e2e

import (
	"testing"

	"maunium.net/go/mautrix/event"
)

// TestSmokeEncryptedDM: bob opens an encrypted DM with alice, they exchange
// a message each way, and each side decrypts the other's.
func TestSmokeEncryptedDM(t *testing.T) {
	srv := Homeserver(t)
	alice := NewPeer(t, srv, "alice")
	bob := NewPeer(t, srv, "bob")

	room := bob.CreateDM(t, alice.UserID, true)
	alice.Join(t, room)
	bob.WaitMember(t, room, alice.UserID, event.MembershipJoin)

	bob.SendText(t, room, "hello alice")
	got := alice.WaitText(t, room, "hello alice")
	if got.Sender != bob.UserID {
		t.Errorf("sender = %s, want %s", got.Sender, bob.UserID)
	}
	if !got.Mautrix.WasEncrypted {
		t.Errorf("alice's copy of bob's message was not decrypted: %+v", got)
	}

	alice.SendText(t, room, "hello bob")
	got = bob.WaitText(t, room, "hello bob")
	if !got.Mautrix.WasEncrypted {
		t.Errorf("bob's copy of alice's message was not decrypted: %+v", got)
	}
}
