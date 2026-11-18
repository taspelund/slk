package mxfmt

import (
	"strings"

	"maunium.net/go/mautrix/id"
)

// fakeResolver is an in-memory Resolver: two known users, one known
// room, and minted aliases for anything else, as ident.Mapper does.
type fakeResolver struct {
	userAlias map[id.UserID]string
	roomAlias map[id.RoomID]string
	names     map[id.UserID]string
	rooms     map[id.RoomID]string
}

func newFakeResolver() *fakeResolver {
	return &fakeResolver{
		userAlias: map[id.UserID]string{
			"@alice:example.org": "UALICE0001",
			"@bob:example.org":   "UBOB000002",
		},
		roomAlias: map[id.RoomID]string{
			"!general:example.org": "CGENERAL01",
		},
		names: map[id.UserID]string{
			"@alice:example.org": "Alice",
			"@bob:example.org":   "Bob <Smith>",
		},
		rooms: map[id.RoomID]string{
			"!general:example.org": "general",
		},
	}
}

func (f *fakeResolver) UserAlias(user id.UserID) string {
	if a, ok := f.userAlias[user]; ok {
		return a
	}
	a := "UMINT" + strings.Repeat("0", 4) + string(rune('A'+len(f.userAlias)))
	f.userAlias[user] = a
	return a
}

func (f *fakeResolver) RoomAlias(room id.RoomID) string {
	if a, ok := f.roomAlias[room]; ok {
		return a
	}
	a := "CMINT" + strings.Repeat("0", 4) + string(rune('A'+len(f.roomAlias)))
	f.roomAlias[room] = a
	return a
}

func (f *fakeResolver) MXID(alias string) (id.UserID, bool) {
	for u, a := range f.userAlias {
		if a == alias {
			return u, true
		}
	}
	return "", false
}

func (f *fakeResolver) RoomID(alias string) (id.RoomID, bool) {
	for r, a := range f.roomAlias {
		if a == alias {
			return r, true
		}
	}
	return "", false
}

func (f *fakeResolver) DisplayName(user id.UserID) string { return f.names[user] }
func (f *fakeResolver) RoomName(room id.RoomID) string    { return f.rooms[room] }

// userNames and channelNames are the alias → name maps slk's renderer
// takes.
func (f *fakeResolver) userNames() map[string]string {
	m := map[string]string{}
	for u, a := range f.userAlias {
		if n := f.names[u]; n != "" {
			m[a] = n
		}
	}
	return m
}

func (f *fakeResolver) channelNames() map[string]string {
	m := map[string]string{}
	for r, a := range f.roomAlias {
		if n := f.rooms[r]; n != "" {
			m[a] = n
		}
	}
	return m
}

func (f *fakeResolver) lookupUser(alias string) (string, bool) {
	n, ok := f.userNames()[alias]
	return n, ok
}

func (f *fakeResolver) lookupChannel(alias string) (string, bool) {
	n, ok := f.channelNames()[alias]
	return n, ok
}

var _ Resolver = (*fakeResolver)(nil)
