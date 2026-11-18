package ident

import (
	"context"
	"fmt"
	"math/rand"
	"regexp"
	"sync"
	"testing"

	"maunium.net/go/mautrix/id"
)

var aliasShape = regexp.MustCompile(`^[UC][A-Z0-9]{10}$`)

func TestAliasShapeAndStability(t *testing.T) {
	ctx := context.Background()
	users := []id.UserID{"@alice:example.org", "@bob:matrix.org", "@b:x", "@🙂:example.org"}
	rooms := []id.RoomID{"!abc:example.org", "!xyz:matrix.org"}
	p := newMem()
	m := New(p)
	other := New(newMem()) // a different store: aliases are derived, not random
	for _, u := range users {
		a, err := m.UserAlias(ctx, u)
		if err != nil {
			t.Fatal(err)
		}
		if !aliasShape.MatchString(a) || a[0] != 'U' {
			t.Errorf("UserAlias(%s) = %q, want U + 10 of [A-Z0-9]", u, a)
		}
		if again, _ := m.UserAlias(ctx, u); again != a {
			t.Errorf("UserAlias(%s) changed: %q then %q", u, a, again)
		}
		if b, _ := New(p).UserAlias(ctx, u); b != a {
			t.Errorf("after restart UserAlias(%s) = %q, want %q", u, b, a)
		}
		if b, _ := other.UserAlias(ctx, u); b != a {
			t.Errorf("in another store UserAlias(%s) = %q, want %q", u, b, a)
		}
	}
	for _, r := range rooms {
		a, err := m.RoomAlias(ctx, r)
		if err != nil {
			t.Fatal(err)
		}
		if !aliasShape.MatchString(a) || a[0] != 'C' {
			t.Errorf("RoomAlias(%s) = %q, want C + 10 of [A-Z0-9]", r, a)
		}
		if b, _ := New(p).RoomAlias(ctx, r); b != a {
			t.Errorf("after restart RoomAlias(%s) = %q, want %q", r, b, a)
		}
	}
}

func TestAliasRoundTripChecksKind(t *testing.T) {
	ctx := context.Background()
	m := New(newMem())
	user := id.UserID("@alice:example.org")
	room := id.RoomID("!abc:example.org")
	ua, _ := m.UserAlias(ctx, user)
	ra, _ := m.RoomAlias(ctx, room)

	if got, ok, err := m.MXID(ctx, ua); err != nil || !ok || got != user {
		t.Errorf("MXID(%s) = %s, %v, %v; want %s", ua, got, ok, err, user)
	}
	if got, ok, err := m.RoomID(ctx, ra); err != nil || !ok || got != room {
		t.Errorf("RoomID(%s) = %s, %v, %v; want %s", ra, got, ok, err, room)
	}
	if got, ok, err := m.MXID(ctx, ra); err != nil || ok {
		t.Errorf("MXID(room alias) = %s, %v, %v; want not found", got, ok, err)
	}
	if got, ok, err := m.RoomID(ctx, ua); err != nil || ok {
		t.Errorf("RoomID(user alias) = %s, %v, %v; want not found", got, ok, err)
	}
	// A user alias stored under the room kind (a store bug, or a forged
	// "U" alias) is still refused by kind.
	p := newMem()
	p.aliases[aliasKey{KindRoom, "!r:x"}] = "UAAAAAAAAAA"
	p.owners["UAAAAAAAAAA"] = aliasKey{KindRoom, "!r:x"}
	if got, ok, err := New(p).MXID(ctx, "UAAAAAAAAAA"); err != nil || ok {
		t.Errorf("MXID of a room-kind alias = %s, %v, %v; want not found", got, ok, err)
	}
	for _, unknown := range []string{"", "U", "UAAAAAAAAAA", "C2222222222", "USLACKBOT", "B0123456789"} {
		if got, ok, err := m.MXID(ctx, unknown); err != nil || ok {
			t.Errorf("MXID(%q) = %s, %v, %v; want not found", unknown, got, ok, err)
		}
		if got, ok, err := m.RoomID(ctx, unknown); err != nil || ok {
			t.Errorf("RoomID(%q) = %s, %v, %v; want not found", unknown, got, ok, err)
		}
	}
	if _, err := m.UserAlias(ctx, ""); err == nil {
		t.Error("UserAlias(\"\"): want an error")
	}
}

func TestAliasCollisionResolvesDeterministically(t *testing.T) {
	ctx := context.Background()
	user := id.UserID("@alice:example.org")
	first := aliasCandidate(KindUser, string(user), 0)
	second := aliasCandidate(KindUser, string(user), 1)
	if first == second {
		t.Fatal("candidates 0 and 1 are equal")
	}

	t.Run("held by another ID", func(t *testing.T) {
		for range 2 { // two independent stores, same answer
			p := newMem()
			if err := p.PutAlias(ctx, KindUser, "@mallory:example.org", first); err != nil {
				t.Fatal(err)
			}
			m := New(p)
			a, err := m.UserAlias(ctx, user)
			if err != nil || a != second {
				t.Fatalf("UserAlias = %q, %v; want the second candidate %q", a, err, second)
			}
			if got, ok, _ := m.MXID(ctx, a); !ok || got != user {
				t.Errorf("MXID(%s) = %s, %v; want %s", a, got, ok, user)
			}
			if got, ok, _ := m.MXID(ctx, first); !ok || got != "@mallory:example.org" {
				t.Errorf("MXID(%s) = %s, %v; want the earlier owner", first, got, ok)
			}
			if b, _ := New(p).UserAlias(ctx, user); b != second {
				t.Errorf("after restart UserAlias = %q, want %q", b, second)
			}
		}
	})

	t.Run("store reports taken", func(t *testing.T) {
		p := newMem()
		p.refuseAlias[first] = true
		p.refuseAlias[second] = true
		a, err := New(p).UserAlias(ctx, user)
		if want := aliasCandidate(KindUser, string(user), 2); err != nil || a != want {
			t.Errorf("UserAlias = %q, %v; want the third candidate %q", a, err, want)
		}
	})
}

func TestManyUsersGetDistinctAliases(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	m := New(p)
	rng := rand.New(rand.NewSource(1))
	const n = 100_000
	seen := make(map[string]string, n)
	for i := range n {
		u := fmt.Sprintf("@%x-%d:%x.example", rng.Uint64(), i, rng.Uint32())
		a, err := m.UserAlias(ctx, id.UserID(u))
		if err != nil {
			t.Fatal(err)
		}
		if !aliasShape.MatchString(a) || a[0] != 'U' {
			t.Fatalf("UserAlias(%s) = %q", u, a)
		}
		if prev, ok := seen[a]; ok {
			t.Fatalf("%s and %s share alias %s", prev, u, a)
		}
		seen[a] = u
	}
	// Collisions are resolved by the store, so also check the raw first
	// candidates: no two of these IDs needed a second one.
	if len(p.owners) != n {
		t.Errorf("store holds %d aliases, want %d", len(p.owners), n)
	}
	for a, u := range seen {
		if aliasCandidate(KindUser, u, 0) != a {
			t.Errorf("%s needed a second candidate", u)
		}
	}
}

func TestConcurrentAliasMintingAgrees(t *testing.T) {
	ctx := context.Background()
	m := New(newMem())
	var wg sync.WaitGroup
	got := make([]string, 32)
	for i := range got {
		wg.Go(func() {
			a, err := m.UserAlias(ctx, "@same:example.org")
			if err != nil {
				t.Error(err)
			}
			got[i] = a
		})
	}
	wg.Wait()
	for _, a := range got {
		if a != got[0] {
			t.Fatalf("aliases differ: %q", got)
		}
	}
}
