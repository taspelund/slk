package rooms

import (
	"context"

	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/mxfmt"
)

// resolver is the Model's mxfmt.Resolver. User aliases come from the
// model's cache (every user it has seen), room aliases from its rooms;
// anything else goes to ident, which mints a new alias or looks one up.
type resolver struct{ m *Model }

var _ mxfmt.Resolver = resolver{}

func (r resolver) UserAlias(user id.UserID) string {
	a, err := r.m.userAlias(context.Background(), user)
	if err != nil {
		return ""
	}
	return a
}

func (r resolver) RoomAlias(room id.RoomID) string {
	r.m.mu.RLock()
	rm := r.m.rooms[room]
	r.m.mu.RUnlock()
	if rm != nil {
		return rm.alias
	}
	a, err := r.m.ids.RoomAlias(context.Background(), room)
	if err != nil {
		return ""
	}
	return a
}

func (r resolver) MXID(alias string) (id.UserID, bool) {
	r.m.aliasMu.Lock()
	u, ok := r.m.aliasUser[alias]
	r.m.aliasMu.Unlock()
	if ok {
		return u, true
	}
	u, ok, err := r.m.ids.MXID(context.Background(), alias)
	return u, ok && err == nil
}

func (r resolver) RoomID(alias string) (id.RoomID, bool) {
	r.m.mu.RLock()
	rid, ok := r.m.byAlias[alias]
	r.m.mu.RUnlock()
	if ok {
		return rid, true
	}
	rid, ok, err := r.m.ids.RoomID(context.Background(), alias)
	return rid, ok && err == nil
}

func (r resolver) DisplayName(user id.UserID) string {
	return r.m.DisplayName("", user)
}

// RoomName is the room's name, or its ID for a room the model does not
// know.
func (r resolver) RoomName(room id.RoomID) string {
	if rm, ok := r.m.RoomByID(room); ok && rm.Name != "" {
		return rm.Name
	}
	return string(room)
}
