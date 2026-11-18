package ident

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"testing/quick"
	"time"

	"maunium.net/go/mautrix/id"
)

func TestInsertLandsBetweenNeighbours(t *testing.T) {
	ctx := context.Background()
	m := New(newMem())
	ok := tsOK(t)
	for i, o := range []int64{1000, 2000, 3000} {
		ok(m.TSForLive(ctx, room, id.EventID(fmt.Sprintf("$l%d", i)), ms(o)))
	}
	if got := ok(m.TSForInsert(ctx, room, "$a", ms(2500))); got != formatTS(2_500_000) {
		t.Fatalf("insert at 2500 = %s", got)
	}
	// Same millisecond as a stored event: right after it.
	if got := ok(m.TSForInsert(ctx, room, "$b", ms(2000))); got != formatTS(2_000_001) {
		t.Fatalf("insert at 2000 = %s", got)
	}
	if got := ok(m.TSForInsert(ctx, room, "$c", ms(2000))); got != formatTS(2_000_002) {
		t.Fatalf("second insert at 2000 = %s", got)
	}
	// Before the oldest and after the newest: the natural TS.
	if got := ok(m.TSForInsert(ctx, room, "$d", ms(500))); got != formatTS(500_000) {
		t.Fatalf("insert at 500 = %s", got)
	}
	if got := ok(m.TSForInsert(ctx, room, "$e", ms(5000))); got != formatTS(5_000_000) {
		t.Fatalf("insert at 5000 = %s", got)
	}
	// Live and backfill respect inserted events as bounds.
	if got := ok(m.TSForLive(ctx, room, "$f", ms(4000))); got != formatTS(5_000_001) {
		t.Fatalf("live after an insert = %s", got)
	}
	if got := ok(m.TSForBackfill(ctx, room, "$g", ms(900))); got != formatTS(499_999) {
		t.Fatalf("backfill after an insert = %s", got)
	}
	// Mapped events keep their TS whatever the method or origin.
	if got := ok(m.TSForInsert(ctx, room, "$a", ms(1))); got != formatTS(2_500_000) {
		t.Fatalf("repeat insert = %s", got)
	}
	if got := ok(m.TSForInsert(ctx, room, "$l0", ms(9999))); got != formatTS(1_000_000) {
		t.Fatalf("insert of a live event = %s", got)
	}
}

func TestInsertFullMillisecondSpillsOver(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	for i := range 1000 {
		p.put(room, id.EventID(fmt.Sprintf("$x%d", i)), formatTS(7_000_000+int64(i)))
	}
	m := New(p)
	if got := tsOK(t)(m.TSForInsert(ctx, room, "$new", ms(7000))); got != formatTS(7_001_000) {
		t.Fatalf("insert into a full millisecond = %s", got)
	}
}

func TestInsertRetriesTakenCandidates(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	p.refuseTS[formatTS(3_000_000)] = 1
	p.refuseTS[formatTS(3_000_001)] = 1
	m := New(p)
	if got := tsOK(t)(m.TSForInsert(ctx, room, "$a", ms(3000))); got != formatTS(3_000_002) {
		t.Fatalf("insert with refused candidates = %s", got)
	}
	p.refuseAllTS = true
	if _, err := m.TSForInsertBetween(ctx, room, "$b", ms(3000), formatTS(3_000_010), formatTS(3_000_020)); err == nil {
		t.Fatal("every candidate refused, no error")
	}
}

func TestInsertBetween(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	m := New(p)
	ok := tsOK(t)
	after, before := formatTS(1_000_500), formatTS(1_000_504)
	// Origin above the bounds is clamped below before.
	if got := ok(m.TSForInsertBetween(ctx, room, "$a", ms(9000), after, before)); got != formatTS(1_000_503) {
		t.Fatalf("clamped high = %s", got)
	}
	// Then the scan goes down.
	if got := ok(m.TSForInsertBetween(ctx, room, "$b", ms(9000), after, before)); got != formatTS(1_000_502) {
		t.Fatalf("second = %s", got)
	}
	// Origin below is clamped above after, and scans up.
	if got := ok(m.TSForInsertBetween(ctx, room, "$c", ms(0), after, before)); got != formatTS(1_000_501) {
		t.Fatalf("clamped low = %s", got)
	}
	if _, err := m.TSForInsertBetween(ctx, room, "$d", ms(1000), after, before); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("full range: %v, want ErrNoRoom", err)
	}
	if _, err := m.TSForInsertBetween(ctx, room, "$d", ms(1000), after, formatTS(1_000_501)); !errors.Is(err, ErrNoRoom) {
		t.Fatalf("adjacent bounds: %v, want ErrNoRoom", err)
	}
	// Open bounds.
	if got := ok(m.TSForInsertBetween(ctx, room, "$e", ms(2000), "", "")); got != formatTS(2_000_000) {
		t.Fatalf("unbounded = %s", got)
	}
	if got := ok(m.TSForInsertBetween(ctx, room, "$f", ms(2000), "", formatTS(2_000_000))); got != formatTS(1_999_999) {
		t.Fatalf("only before = %s", got)
	}
	if _, err := m.TSForInsertBetween(ctx, room, "$g", ms(1), "bad", ""); err == nil {
		t.Fatal("malformed bound accepted")
	}
	if got := ok(m.TSForInsertBetween(ctx, room, "$a", ms(1), "", formatTS(1))); got != formatTS(1_000_503) {
		t.Fatalf("mapped event ignores bounds: %s", got)
	}
}

// insertHistory is a random interleaving of live, backfilled and inserted
// events in a few rooms, with repeats of earlier events.
type insertHistory []op

const opInsert = numOpKinds // reuses op; kinds live, backfill, insert, repeat

func (insertHistory) Generate(r *rand.Rand, size int) reflect.Value {
	h := history{}.Generate(r, size).Interface().(history)
	for i := range h {
		h[i].kind = []opKind{opLive, opBackfill, opInsert, opInsert, opRepeat}[r.Intn(5)]
	}
	return reflect.ValueOf(insertHistory(h))
}

func checkInsertHistory(t *testing.T, h insertHistory) bool {
	t.Helper()
	ctx := context.Background()
	p := newMem()
	m := New(p)
	type key struct {
		room int
		ev   id.EventID
	}
	got := map[key]int64{}
	var order []key
	byRoom := make([]map[int64]bool, historyRooms)
	for i := range byRoom {
		byRoom[i] = map[int64]bool{}
	}
	roomID := func(i int) id.RoomID { return id.RoomID(fmt.Sprintf("!room%d:example.org", i)) }
	fail := func(format string, args ...any) bool {
		t.Errorf(format, args...)
		return false
	}
	for i, o := range h {
		r := roomID(o.room)
		if o.kind == opRepeat {
			if len(order) == 0 {
				continue
			}
			k := order[o.pick%len(order)]
			ts, err := []func(context.Context, id.RoomID, id.EventID, time.Time) (string, error){
				m.TSForLive, m.TSForBackfill, m.TSForInsert,
			}[o.method](ctx, roomID(k.room), k.ev, ms(o.origin))
			if err != nil || mustParse(t, ts) != got[k] {
				return fail("op %d: repeat of %s = %q, %v; want %s", i, k.ev, ts, err, formatTS(got[k]))
			}
			continue
		}
		ev := id.EventID(fmt.Sprintf("$e%d", i))
		var ts string
		var err error
		switch o.kind {
		case opLive:
			ts, err = m.TSForLive(ctx, r, ev, ms(o.origin))
		case opBackfill:
			ts, err = m.TSForBackfill(ctx, r, ev, ms(o.origin))
		case opInsert:
			ts, err = m.TSForInsert(ctx, r, ev, ms(o.origin))
		}
		if err != nil {
			return fail("op %d: %v", i, err)
		}
		us := mustParse(t, ts)
		taken := byRoom[o.room]
		if taken[us] {
			return fail("op %d: TS %s used twice in room %d", i, ts, o.room)
		}
		for other := range taken {
			if (o.kind == opLive && other >= us) || (o.kind == opBackfill && other <= us) {
				return fail("op %d (%v): TS %s not past stored %s", i, o.kind, ts, formatTS(other))
			}
		}
		if o.kind == opInsert && us/1000 != o.origin {
			// Every millisecond has free slots in these histories.
			return fail("op %d: insert at %d got %s, outside its millisecond", i, o.origin, ts)
		}
		taken[us] = true
		k := key{o.room, ev}
		got[k] = us
		order = append(order, k)
	}
	restarted := New(p)
	for _, k := range order {
		if ts, err := restarted.TSForInsert(ctx, roomID(k.room), k.ev, ms(0)); err != nil || mustParse(t, ts) != got[k] {
			return fail("after restart, TSForInsert(%s) = %q, %v; want %s", k.ev, ts, err, formatTS(got[k]))
		}
	}
	return true
}

func TestInsertPropertiesHoldForRandomHistories(t *testing.T) {
	if err := quick.Check(func(h insertHistory) bool { return checkInsertHistory(t, h) }, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}
