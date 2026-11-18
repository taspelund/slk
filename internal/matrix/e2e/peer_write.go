//go:build matrixe2e

package e2e

import (
	"encoding/json"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Typing returns who the server says is typing in room now, as the peer
// sees it. It asks with a sync of its own (no since token, so it does not
// move the peer's sync along), filtered to room's typing notifications.
func (p *Peer) Typing(t testing.TB, room id.RoomID) []id.UserID {
	t.Helper()
	filter, err := json.Marshal(&mautrix.Filter{
		AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		Presence:    &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
		Room: &mautrix.RoomFilter{
			Rooms:       []id.RoomID{room},
			Timeline:    &mautrix.FilterPart{Limit: 1},
			State:       &mautrix.FilterPart{LazyLoadMembers: true},
			AccountData: &mautrix.FilterPart{NotTypes: []event.Type{{Type: "*"}}},
			Ephemeral:   &mautrix.FilterPart{Types: []event.Type{event.EphemeralEventTyping}},
		},
	})
	if err != nil {
		t.Fatalf("e2e: typing filter: %v", err)
	}
	resp, err := p.Client.FullSyncRequest(Ctx(t), mautrix.ReqSync{FilterID: string(filter)})
	if err != nil {
		t.Fatalf("e2e: %s: sync for typing: %v", p.UserID, err)
	}
	jr := resp.Rooms.Join[room]
	if jr == nil {
		return nil
	}
	var out []id.UserID
	for _, evt := range jr.Ephemeral.Events {
		if evt.Type.Type != event.EphemeralEventTyping.Type {
			continue
		}
		var c event.TypingEventContent
		if err := json.Unmarshal(evt.Content.VeryRaw, &c); err == nil {
			out = append(out, c.UserIDs...)
		}
	}
	return out
}
