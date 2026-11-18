package ident

import (
	"context"
	"fmt"
	"sync"

	"maunium.net/go/mautrix/id"
)

// memPersistence is an in-memory Persistence for tests. Beyond the
// interface's contract it refuses to map one event to a second TS, or one
// Matrix ID to a second alias, so a Mapper that tries either fails loudly.
type memPersistence struct {
	mu      sync.Mutex
	ts      map[id.RoomID]map[id.EventID]string
	events  map[id.RoomID]map[string]id.EventID
	bounds  map[id.RoomID][2]string // oldest, newest
	aliases map[aliasKey]string
	owners  map[string]aliasKey

	// refuseTS lists TS values PutTS answers with ErrTSTaken while their
	// count is positive (decremented per refusal), as if another writer
	// held them. refuseAllTS refuses every PutTS.
	refuseTS    map[string]int
	refuseAllTS bool
	// refuseAlias lists aliases PutAlias answers with ErrAliasTaken, as if
	// another Matrix ID held them.
	refuseAlias map[string]bool
	putTSCalls  int
}

type aliasKey struct {
	kind     Kind
	matrixID string
}

var _ Persistence = (*memPersistence)(nil)

func newMem() *memPersistence {
	return &memPersistence{
		ts:          map[id.RoomID]map[id.EventID]string{},
		events:      map[id.RoomID]map[string]id.EventID{},
		bounds:      map[id.RoomID][2]string{},
		aliases:     map[aliasKey]string{},
		owners:      map[string]aliasKey{},
		refuseTS:    map[string]int{},
		refuseAlias: map[string]bool{},
	}
}

func (p *memPersistence) LookupTS(_ context.Context, room id.RoomID, ev id.EventID) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ts, ok := p.ts[room][ev]
	return ts, ok, nil
}

func (p *memPersistence) LookupEvent(_ context.Context, room id.RoomID, ts string) (id.EventID, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	ev, ok := p.events[room][ts]
	return ev, ok, nil
}

func (p *memPersistence) RoomBounds(_ context.Context, room id.RoomID) (string, string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	b, ok := p.bounds[room]
	return b[0], b[1], ok, nil
}

func (p *memPersistence) PutTS(_ context.Context, room id.RoomID, ev id.EventID, ts string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.putTSCalls++
	if p.refuseAllTS {
		return ErrTSTaken
	}
	if p.refuseTS[ts] > 0 {
		p.refuseTS[ts]--
		return ErrTSTaken
	}
	if other, ok := p.events[room][ts]; ok {
		if other != ev {
			return ErrTSTaken
		}
		return nil
	}
	if old, ok := p.ts[room][ev]; ok {
		return fmt.Errorf("memPersistence: %s already mapped to %s, not %s", ev, old, ts)
	}
	p.putLocked(room, ev, ts)
	return nil
}

// put records ev <-> ts with no checks, for seeding a room's bounds
// (including malformed values a real store should never hold).
func (p *memPersistence) put(room id.RoomID, ev id.EventID, ts string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.putLocked(room, ev, ts)
}

func (p *memPersistence) putLocked(room id.RoomID, ev id.EventID, ts string) {
	if p.ts[room] == nil {
		p.ts[room] = map[id.EventID]string{}
		p.events[room] = map[string]id.EventID{}
	}
	p.ts[room][ev] = ts
	p.events[room][ts] = ev
	b, ok := p.bounds[room]
	if !ok {
		b = [2]string{ts, ts}
	}
	b[0] = min(b[0], ts)
	b[1] = max(b[1], ts)
	p.bounds[room] = b
}

func (p *memPersistence) LookupAlias(_ context.Context, kind Kind, matrixID string) (string, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	a, ok := p.aliases[aliasKey{kind, matrixID}]
	return a, ok, nil
}

func (p *memPersistence) LookupMatrixID(_ context.Context, alias string) (string, Kind, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	k, ok := p.owners[alias]
	return k.matrixID, k.kind, ok, nil
}

func (p *memPersistence) PutAlias(_ context.Context, kind Kind, matrixID, alias string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := aliasKey{kind, matrixID}
	if p.refuseAlias[alias] {
		return ErrAliasTaken
	}
	if owner, ok := p.owners[alias]; ok {
		if owner != key {
			return ErrAliasTaken
		}
		return nil
	}
	if old, ok := p.aliases[key]; ok {
		return fmt.Errorf("memPersistence: %s already has alias %s, not %s", matrixID, old, alias)
	}
	p.aliases[key] = alias
	p.owners[alias] = key
	return nil
}
