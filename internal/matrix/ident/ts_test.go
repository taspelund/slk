package ident

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"regexp"
	"slices"
	"sync"
	"testing"
	"testing/quick"
	"time"

	"maunium.net/go/mautrix/id"
)

var tsShape = regexp.MustCompile(`^\d{10}\.\d{6}$`)

const room = id.RoomID("!r:example.org")

func ms(n int64) time.Time { return time.UnixMilli(n) }

func mustTS(t *testing.T, ts string, err error) string {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	if !tsShape.MatchString(ts) {
		t.Fatalf("TS %q does not match %s", ts, tsShape)
	}
	return ts
}

// tsOK returns mustTS bound to t, for wrapping a two-value mint call.
func tsOK(t *testing.T) func(string, error) string {
	return func(ts string, err error) string {
		t.Helper()
		return mustTS(t, ts, err)
	}
}

func TestEmptyRoomTakesNaturalTS(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		mint func(*Mapper) (string, error)
	}{
		{"live", func(m *Mapper) (string, error) { return m.TSForLive(ctx, room, "$e", ms(1700000000123)) }},
		{"backfill", func(m *Mapper) (string, error) { return m.TSForBackfill(ctx, room, "$e", ms(1700000000123)) }},
		{"sent", func(m *Mapper) (string, error) { return m.TSForSent(ctx, room, "$e", ms(1700000000123)) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := tc.mint(New(newMem()))
			if got := mustTS(t, ts, err); got != "1700000000.123000" {
				t.Errorf("TS = %q, want 1700000000.123000", got)
			}
		})
	}
}

func TestSameMillisecondUsesTiebreak(t *testing.T) {
	ctx := context.Background()
	m := New(newMem())
	var got []string
	for i := range 3 {
		ts, err := m.TSForLive(ctx, room, id.EventID(fmt.Sprint("$l", i)), ms(1700000000123))
		got = append(got, mustTS(t, ts, err))
	}
	for i := range 2 {
		ts, err := m.TSForBackfill(ctx, room, id.EventID(fmt.Sprint("$b", i)), ms(1700000000123))
		got = append(got, mustTS(t, ts, err))
	}
	want := []string{"1700000000.123000", "1700000000.123001", "1700000000.123002", "1700000000.122999", "1700000000.122998"}
	if !slices.Equal(got, want) {
		t.Errorf("TS values = %q, want %q", got, want)
	}
}

func TestBumpCarriesAcrossSeconds(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	p.put(room, "$a", "0000000005.999999")
	p.put(room, "$b", "0000000006.000000")
	m := New(p)
	ts, err := m.TSForLive(ctx, room, "$live", ms(1000))
	if got := mustTS(t, ts, err); got != "0000000006.000001" {
		t.Errorf("live = %q, want 0000000006.000001", got)
	}

	p = newMem()
	p.put(room, "$a", "0000000005.999999")
	m = New(p)
	ts, err = m.TSForLive(ctx, room, "$live", ms(1000))
	if got := mustTS(t, ts, err); got != "0000000006.000000" {
		t.Errorf("live after .999999 = %q, want 0000000006.000000", got)
	}

	p = newMem()
	p.put(room, "$a", "0000000006.000000")
	m = New(p)
	ts, err = m.TSForBackfill(ctx, room, "$old", ms(9000))
	if got := mustTS(t, ts, err); got != "0000000005.999999" {
		t.Errorf("backfill before .000000 = %q, want 0000000005.999999", got)
	}
}

func TestBumpOutOfRangeIsAnError(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	p.put(room, "$zero", "0000000000.000000")
	m := New(p)
	if ts, err := m.TSForBackfill(ctx, room, "$older", ms(5)); err == nil {
		t.Errorf("backfill below zero = %q, want an error", ts)
	}
	if _, ok, _ := p.LookupTS(ctx, room, "$older"); ok {
		t.Error("a refused backfill was persisted")
	}

	p = newMem()
	p.put(room, "$max", "9999999999.999999")
	m = New(p)
	if ts, err := m.TSForLive(ctx, room, "$newer", ms(5)); err == nil {
		t.Errorf("live above the maximum = %q, want an error", ts)
	}
}

func TestOriginOutsideRangeIsClamped(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name   string
		origin time.Time
		want   string
	}{
		{"before epoch", ms(-5000), "0000000000.000000"},
		{"zero time", time.Time{}, "0000000000.000000"},
		{"after year 2286", time.Unix(99_999_999_999, 0), "9999999999.999000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts, err := New(newMem()).TSForLive(ctx, room, "$e", tc.origin)
			if got := mustTS(t, ts, err); got != tc.want {
				t.Errorf("TS = %q, want %q", got, tc.want)
			}
		})
	}
	// A clamped origin is still bumped into order.
	ok := tsOK(t)
	m := New(newMem())
	ok(m.TSForLive(ctx, room, "$a", ms(1700000000000)))
	ts, err := m.TSForLive(ctx, room, "$b", ms(-1))
	if got := mustTS(t, ts, err); got != "1700000000.000001" {
		t.Errorf("pre-epoch live after a real one = %q, want 1700000000.000001", got)
	}
}

func TestTakenCandidateAdvances(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	p.refuseTS["1700000000.123000"] = 2
	m := New(p)
	ts, err := m.TSForLive(ctx, room, "$l", ms(1700000000123))
	if got := mustTS(t, ts, err); got != "1700000000.123001" {
		t.Errorf("live = %q, want 1700000000.123001", got)
	}
	ts, err = m.TSForBackfill(ctx, room, "$b", ms(1700000000123))
	if got := mustTS(t, ts, err); got != "1700000000.122999" {
		t.Errorf("backfill = %q, want 1700000000.122999 (.123000 refused once more)", got)
	}
	if p.refuseTS["1700000000.123000"] != 0 {
		t.Error("the refused candidate was not retried")
	}
}

func TestAlwaysTakenGivesUp(t *testing.T) {
	p := newMem()
	p.refuseAllTS = true
	if ts, err := New(p).TSForLive(context.Background(), room, "$e", ms(1)); err == nil {
		t.Fatalf("TS = %q, want an error", ts)
	}
	if p.putTSCalls != maxAllocAttempts {
		t.Errorf("PutTS called %d times, want %d", p.putTSCalls, maxAllocAttempts)
	}
}

func TestMalformedBoundsIsAnError(t *testing.T) {
	p := newMem()
	p.put(room, "$bad", "17000.1")
	if ts, err := New(p).TSForLive(context.Background(), room, "$e", ms(1)); err == nil {
		t.Errorf("TS = %q, want an error for a malformed stored TS", ts)
	}
}

func TestEmptyIDsAreErrors(t *testing.T) {
	ctx := context.Background()
	m := New(newMem())
	if _, err := m.TSForLive(ctx, "", "$e", ms(1)); err == nil {
		t.Error("empty room: want an error")
	}
	if _, err := m.TSForBackfill(ctx, room, "", ms(1)); err == nil {
		t.Error("empty event: want an error")
	}
}

func TestStoreErrorsPropagate(t *testing.T) {
	boom := errors.New("boom")
	m := New(failingPersistence{boom})
	if _, err := m.TSForLive(context.Background(), room, "$e", ms(1)); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
	if _, err := m.UserAlias(context.Background(), "@u:x"); !errors.Is(err, boom) {
		t.Errorf("alias err = %v, want %v", err, boom)
	}
}

type failingPersistence struct{ err error }

func (f failingPersistence) LookupTS(context.Context, id.RoomID, id.EventID) (string, bool, error) {
	return "", false, f.err
}
func (f failingPersistence) LookupEvent(context.Context, id.RoomID, string) (id.EventID, bool, error) {
	return "", false, f.err
}
func (f failingPersistence) RoomBounds(context.Context, id.RoomID) (string, string, bool, error) {
	return "", "", false, f.err
}
func (f failingPersistence) PutTS(context.Context, id.RoomID, id.EventID, string) error { return f.err }
func (f failingPersistence) LookupAlias(context.Context, Kind, string) (string, bool, error) {
	return "", false, f.err
}
func (f failingPersistence) LookupMatrixID(context.Context, string) (string, Kind, bool, error) {
	return "", 0, false, f.err
}
func (f failingPersistence) PutAlias(context.Context, Kind, string, string) error { return f.err }

func TestSentThenEchoKeepsTS(t *testing.T) {
	ctx := context.Background()
	ok := tsOK(t)
	m := New(newMem())
	ok(m.TSForLive(ctx, room, "$before", ms(1700000000500)))
	sent := ok(m.TSForSent(ctx, room, "$mine", ms(1700000000400)))
	// Another event arrives before the echo; the echo keeps its TS.
	ok(m.TSForLive(ctx, room, "$other", ms(1700000000600)))
	echo := ok(m.TSForLive(ctx, room, "$mine", ms(1700000000450)))
	if echo != sent {
		t.Errorf("echo TS = %q, want the sent TS %q", echo, sent)
	}
	// And the other way round: the echo raced ahead of the send's return.
	live := ok(m.TSForLive(ctx, room, "$fast", ms(1700000000700)))
	if got := ok(m.TSForSent(ctx, room, "$fast", ms(1700000000800))); got != live {
		t.Errorf("sent after echo = %q, want %q", got, live)
	}
	if ev, ok, err := m.Event(ctx, room, sent); err != nil || !ok || ev != "$mine" {
		t.Errorf("Event(%q) = %q, %v, %v; want $mine", sent, ev, ok, err)
	}
	if ts, ok, err := m.TS(ctx, room, "$unknown"); err != nil || ok {
		t.Errorf("TS of an unmapped event = %q, %v, %v; want not found", ts, ok, err)
	}
}

// opKind is one step of a generated room history.
type opKind int

const (
	opLive opKind = iota
	opBackfill
	opSent
	opEcho   // TSForLive for an event already sent
	opRepeat // any TS method for an event already mapped
	numOpKinds
)

type op struct {
	kind   opKind
	room   int
	origin int64 // milliseconds
	pick   int   // which earlier event, for opEcho and opRepeat
	method int   // which method, for opRepeat
}

// history is a random interleaving of live, backfilled and sent events in
// a few rooms, with origin times that repeat, go backwards and jump.
type history []op

const historyRooms = 3

func (history) Generate(r *rand.Rand, size int) reflect.Value {
	n := r.Intn(4*size + 1)
	h := make(history, n)
	clock := int64(1_700_000_000_000)
	for i := range h {
		switch r.Intn(6) {
		case 0: // same millisecond again
		case 1:
			clock += int64(r.Intn(5))
		case 2:
			clock -= int64(r.Intn(5))
		case 3:
			clock += int64(r.Intn(3_600_000))
		case 4:
			clock -= int64(r.Intn(3_600_000))
		case 5: // far apart: up to a year either way
			clock += int64(r.Intn(2*31_536_000_000)) - 31_536_000_000
		}
		h[i] = op{
			kind:   opKind(r.Intn(int(numOpKinds))),
			room:   r.Intn(historyRooms),
			origin: clock,
			pick:   r.Int(),
			method: r.Intn(3),
		}
	}
	return reflect.ValueOf(h)
}

type minted struct {
	ev id.EventID
	ts string
}

// checkHistory replays h through a Mapper, checking every mint against a
// model of each room's timeline, then the invariants over the whole
// result, then that a new Mapper over the same store agrees.
func checkHistory(t *testing.T, h history) bool {
	t.Helper()
	ctx := context.Background()
	p := newMem()
	m := New(p)
	rooms := make([]id.RoomID, historyRooms)
	for i := range rooms {
		rooms[i] = id.RoomID(fmt.Sprintf("!room%d:example.org", i))
	}
	timeline := make([][]minted, historyRooms) // in timeline order
	var all []struct {
		room int
		minted
	}
	var sent []int // indexes into all
	fail := func(format string, args ...any) bool {
		t.Errorf(format, args...)
		return false
	}

	for i, o := range h {
		r := rooms[o.room]
		tl := timeline[o.room]
		switch o.kind {
		case opLive, opBackfill, opSent:
			ev := id.EventID(fmt.Sprintf("$e%d", i))
			var ts string
			var err error
			switch o.kind {
			case opLive:
				ts, err = m.TSForLive(ctx, r, ev, ms(o.origin))
			case opBackfill:
				ts, err = m.TSForBackfill(ctx, r, ev, ms(o.origin))
			case opSent:
				ts, err = m.TSForSent(ctx, r, ev, ms(o.origin))
			}
			if err != nil {
				return fail("op %d: %v", i, err)
			}
			if !tsShape.MatchString(ts) {
				return fail("op %d: TS %q has the wrong shape", i, ts)
			}
			// The model: the natural TS unless it would be out of order,
			// then exactly one microsecond past the bound.
			want := o.origin * 1000
			if len(tl) > 0 {
				if o.kind == opBackfill {
					want = min(want, mustParse(t, tl[0].ts)-1)
				} else {
					want = max(want, mustParse(t, tl[len(tl)-1].ts)+1)
				}
			}
			if got := mustParse(t, ts); got != want {
				return fail("op %d (%v at %d): TS %q, want %q", i, o.kind, o.origin, ts, formatTS(want))
			}
			mt := minted{ev, ts}
			if o.kind == opBackfill {
				timeline[o.room] = append([]minted{mt}, tl...)
			} else {
				timeline[o.room] = append(tl, mt)
			}
			all = append(all, struct {
				room int
				minted
			}{o.room, mt})
			if o.kind == opSent {
				sent = append(sent, len(all)-1)
			}
		case opEcho:
			if len(sent) == 0 {
				continue
			}
			e := all[sent[o.pick%len(sent)]]
			ts, err := m.TSForLive(ctx, rooms[e.room], e.ev, ms(o.origin))
			if err != nil || ts != e.ts {
				return fail("op %d: echo of %s = %q, %v; want %q", i, e.ev, ts, err, e.ts)
			}
		case opRepeat:
			if len(all) == 0 {
				continue
			}
			e := all[o.pick%len(all)]
			ts, err := []func(context.Context, id.RoomID, id.EventID, time.Time) (string, error){
				m.TSForLive, m.TSForBackfill, m.TSForSent,
			}[o.method](ctx, rooms[e.room], e.ev, ms(o.origin))
			if err != nil || ts != e.ts {
				return fail("op %d: repeat of %s = %q, %v; want %q", i, e.ev, ts, err, e.ts)
			}
		}
	}

	for ri, tl := range timeline {
		seen := map[string]bool{}
		for _, e := range tl {
			if seen[e.ts] {
				return fail("room %d: TS %q used twice", ri, e.ts)
			}
			seen[e.ts] = true
		}
		sorted := slices.Clone(tl)
		slices.SortFunc(sorted, func(a, b minted) int {
			switch {
			case a.ts < b.ts:
				return -1
			case a.ts > b.ts:
				return 1
			}
			return 0
		})
		if !slices.Equal(sorted, tl) {
			return fail("room %d: sorting by TS does not give timeline order:\n got %v\nwant %v", ri, sorted, tl)
		}
	}

	restarted := New(p)
	for _, e := range all {
		r := rooms[e.room]
		if ts, ok, err := restarted.TS(ctx, r, e.ev); err != nil || !ok || ts != e.ts {
			return fail("after restart, TS(%s) = %q, %v, %v; want %q", e.ev, ts, ok, err, e.ts)
		}
		if ts, err := restarted.TSForBackfill(ctx, r, e.ev, ms(0)); err != nil || ts != e.ts {
			return fail("after restart, TSForBackfill(%s) = %q, %v; want %q", e.ev, ts, err, e.ts)
		}
		if ev, ok, err := restarted.Event(ctx, r, e.ts); err != nil || !ok || ev != e.ev {
			return fail("after restart, Event(%q) = %s, %v, %v; want %s", e.ts, ev, ok, err, e.ev)
		}
	}
	return true
}

func mustParse(t *testing.T, ts string) int64 {
	t.Helper()
	us, err := parseTS(ts)
	if err != nil {
		t.Fatal(err)
	}
	return us
}

func (k opKind) String() string {
	return [...]string{"live", "backfill", "sent", "echo", "repeat"}[k]
}

func TestTSPropertiesHoldForRandomHistories(t *testing.T) {
	if err := quick.Check(func(h history) bool { return checkHistory(t, h) }, &quick.Config{MaxCount: 500}); err != nil {
		t.Error(err)
	}
}

func TestFormatParseRoundTrip(t *testing.T) {
	f := func(us int64) bool {
		if us < 0 {
			us = -(us + 1)
		}
		us %= maxTSMicros + 1
		ts := formatTS(us)
		got, err := parseTS(ts)
		return err == nil && got == us && tsShape.MatchString(ts)
	}
	if err := quick.Check(f, nil); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{"", "1700000000", "1700000000.12345", "170000000a.123456", "1700000000,123456", "-700000000.123456", "17000000000.12345"} {
		if _, err := parseTS(bad); err == nil {
			t.Errorf("parseTS(%q) succeeded", bad)
		}
	}
}

func TestConcurrentMintingKeepsOrderAndUniqueness(t *testing.T) {
	ctx := context.Background()
	p := newMem()
	m := New(p)
	const (
		rooms   = 4
		writers = 8 // per room and direction
		perG    = 40
		shared  = 10 // events every goroutine of a room asks for at once
	)
	type result struct {
		room    id.RoomID
		ev      id.EventID
		ts      string
		forward bool
	}
	var (
		mu      sync.Mutex
		results []result
		wg      sync.WaitGroup
	)
	errs := make(chan error, rooms*writers*2)
	for ri := range rooms {
		r := id.RoomID(fmt.Sprintf("!c%d:x", ri))
		for g := range 2 * writers {
			forward := g%2 == 0
			wg.Go(func() {
				rng := rand.New(rand.NewSource(int64(ri*100 + g)))
				prev := ""
				var mine []result
				for i := range perG {
					ev := id.EventID(fmt.Sprintf("$%d-%d-%d", ri, g, i))
					origin := ms(1_700_000_000_000 + int64(rng.Intn(20)))
					var ts string
					var err error
					if forward {
						ts, err = m.TSForLive(ctx, r, ev, origin)
					} else {
						ts, err = m.TSForBackfill(ctx, r, ev, origin)
					}
					if err != nil {
						errs <- err
						return
					}
					if prev != "" && (forward && ts <= prev || !forward && ts >= prev) {
						errs <- fmt.Errorf("%s: %s after %s breaks this goroutine's order (forward=%v)", ev, ts, prev, forward)
						return
					}
					prev = ts
					mine = append(mine, result{r, ev, ts, forward})
					if i < shared {
						sev := id.EventID(fmt.Sprintf("$shared-%d-%d", ri, i))
						sts, err := m.TSForLive(ctx, r, sev, origin)
						if err != nil {
							errs <- err
							return
						}
						mine = append(mine, result{r, sev, sts, true})
					}
				}
				mu.Lock()
				results = append(results, mine...)
				mu.Unlock()
			})
		}
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	byEvent := map[id.EventID]string{}
	byTS := map[id.RoomID]map[string]id.EventID{}
	maxBack := map[id.RoomID]string{}
	minLive := map[id.RoomID]string{}
	for _, res := range results {
		if old, ok := byEvent[res.ev]; ok && old != res.ts {
			t.Errorf("%s got two TS values: %s and %s", res.ev, old, res.ts)
		}
		byEvent[res.ev] = res.ts
		if byTS[res.room] == nil {
			byTS[res.room] = map[string]id.EventID{}
		}
		if other, ok := byTS[res.room][res.ts]; ok && other != res.ev {
			t.Errorf("%s: TS %s given to %s and %s", res.room, res.ts, other, res.ev)
		}
		byTS[res.room][res.ts] = res.ev
		if res.forward {
			if v, ok := minLive[res.room]; !ok || res.ts < v {
				minLive[res.room] = res.ts
			}
		} else if res.ts > maxBack[res.room] {
			maxBack[res.room] = res.ts
		}
	}
	// Every backfilled event sorts before every live one: whichever came
	// first in a room, the other kind was bumped past it.
	for r, back := range maxBack {
		if back >= minLive[r] {
			t.Errorf("%s: newest backfill %s is not before oldest live %s", r, back, minLive[r])
		}
	}
	if want := rooms * (2*writers*perG + shared); len(byEvent) != want {
		t.Errorf("%d distinct events, want %d", len(byEvent), want)
	}
}
