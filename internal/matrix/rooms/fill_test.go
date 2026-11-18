package rooms

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"maunium.net/go/mautrix"
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/matrix/store"
)

// live stores events through /sync.
func (f *fixture) live(rid id.RoomID, evts ...*event.Event) {
	f.t.Helper()
	resp := newSync()
	join(resp, rid).Timeline.Events = evts
	f.apply(resp)
}

// inOrder fails unless the events' TS values ascend in the order given.
func (f *fixture) inOrder(rid id.RoomID, evs ...id.EventID) {
	f.t.Helper()
	var tss []string
	for _, ev := range evs {
		tss = append(tss, f.ts(rid, ev))
	}
	if !slices.IsSorted(tss) {
		f.t.Fatalf("TS order of %v = %v, want ascending", evs, tss)
	}
}

func threadReply(t *testing.T, evID, root id.EventID, sender id.UserID, ms int64, body string) *event.Event {
	return mk(t, "m.room.message", evID, sender, ms, nil, map[string]any{
		"msgtype": "m.text", "body": body,
		"m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root},
	})
}

func TestBackfillInsertsThreadRepliesBetweenNeighbours(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	f.live(rid, txt(t, "$m1", alice, 1000, "root"), txt(t, "$m2", alice, 2000, "two"), txt(t, "$m3", alice, 5000, "five"))

	// /relations: newest first, the root (stored) after them.
	ch, err := f.m.Backfill(context.Background(), rid, []*event.Event{
		threadReply(t, "$r2", "$m1", bob, 4000, "second"),
		threadReply(t, "$r1", "$m1", bob, 3000, "first"),
		txt(t, "$m1", alice, 1000, "root"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$m1", "$m2", "$r1", "$r2", "$m3")
	if got := f.ts(rid, "$r1"); got != "0000000003.000000" {
		t.Fatalf("reply TS = %s, want its natural TS", got)
	}
	want := []TimelineChange{
		{Kind: Added, Room: rid, Event: "$r2", TS: f.ts(rid, "$r2"), Thread: "$m1"},
		{Kind: Added, Room: rid, Event: "$r1", TS: f.ts(rid, "$r1"), Thread: "$m1"},
	}
	if !reflect.DeepEqual(ch.Timeline, want) {
		t.Fatalf("Timeline = %+v\nwant %+v", ch.Timeline, want)
	}
	if e := f.event(rid, "$r1"); e.ThreadRoot != "$m1" || e.Body != "first" {
		t.Fatalf("reply row = %+v", e)
	}
	if r, _, _ := f.st.GetRoom(context.Background(), rid); r.LastEventTS != f.ts(rid, "$m3") {
		t.Fatalf("LastEventTS = %s", r.LastEventTS)
	}

	// A thread whose root is older than everything stored, and a reply
	// newer than everything.
	ch, err = f.m.Backfill(context.Background(), rid, []*event.Event{
		threadReply(t, "$r9", "$old", bob, 9000, "late"),
		threadReply(t, "$r8", "$old", bob, 1500, "early"),
		txt(t, "$old", alice, 500, "old root"),
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$old", "$m1", "$r8", "$m2", "$m3", "$r9")
	if len(ch.Timeline) != 3 {
		t.Fatalf("Timeline = %+v", ch.Timeline)
	}
	if r, _, _ := f.st.GetRoom(context.Background(), rid); r.LastEventTS != f.ts(rid, "$r9") {
		t.Fatalf("LastEventTS after a newer insert = %s", r.LastEventTS)
	}
}

func TestBackfillContextPages(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	f.live(rid, txt(t, "$s10", alice, 10_000, "s10"), txt(t, "$s30", alice, 30_000, "s30"),
		txt(t, "$s50", alice, 50_000, "s50"))

	// /context around $t20, which falls between stored events:
	// events_after reversed, the event, events_before.
	page := []*event.Event{
		txt(t, "$s30", alice, 30_000, "s30"),
		txt(t, "$x25", bob, 25_000, "x25"),
		txt(t, "$t20", bob, 20_000, "t20"),
		txt(t, "$x15", bob, 15_000, "x15"),
		txt(t, "$s10", alice, 10_000, "s10"),
	}
	ch, err := f.m.Backfill(context.Background(), rid, page, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$s10", "$x15", "$t20", "$x25", "$s30", "$s50")
	var added []id.EventID
	for _, c := range ch.Timeline {
		if c.Kind == Added && !c.Live {
			added = append(added, c.Event)
		}
	}
	if !slices.Equal(added, []id.EventID{"$x25", "$t20", "$x15"}) {
		t.Fatalf("Added = %v (%+v)", added, ch.Timeline)
	}

	// /context around an event older than everything stored.
	page = []*event.Event{
		txt(t, "$a3", bob, 7_000, "a3"),
		txt(t, "$a2", bob, 6_000, "a2"),
		txt(t, "$t5", bob, 5_000, "t5"),
		txt(t, "$b1", bob, 4_000, "b1"),
	}
	if _, err := f.m.Backfill(context.Background(), rid, page, nil); err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$b1", "$t5", "$a2", "$a3", "$s10")
}

func TestFillGapPartialThenComplete(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	f.live(rid, txt(t, "$o1", alice, 1000, "o1"), txt(t, "$o2", alice, 2000, "o2"))
	resp := newSync()
	jr := join(resp, rid)
	jr.Timeline = mautrix.SyncTimeline{Limited: true, PrevBatch: "p1"}
	jr.Timeline.Events = []*event.Event{txt(t, "$n1", alice, 10_000, "n1"), txt(t, "$n2", alice, 11_000, "n2")}
	f.apply(resp)
	gaps, err := f.st.Gaps(context.Background(), rid)
	if err != nil || len(gaps) != 1 {
		t.Fatalf("gaps = %+v %v", gaps, err)
	}
	gap := gaps[0]

	// Page one, newest first. $g4's origin is later than $n1's (a skewed
	// clock): the page order puts it before $n1 anyway.
	ch, err := f.m.FillGap(context.Background(), rid, gap.ID, []*event.Event{
		txt(t, "$g4", bob, 12_000, "g4"),
		txt(t, "$g3", bob, 8_000, "g3"),
	}, []*event.Event{memberEv(t, bob, "Bob")}, "p2", false)
	if err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$o2", "$g3", "$g4", "$n1")
	want := []TimelineChange{
		{Kind: Added, Room: rid, Event: "$g4", TS: f.ts(rid, "$g4")},
		{Kind: Added, Room: rid, Event: "$g3", TS: f.ts(rid, "$g3")},
	}
	if !reflect.DeepEqual(ch.Timeline, want) {
		t.Fatalf("Timeline = %+v\nwant %+v", ch.Timeline, want)
	}
	if g, ok, err := f.st.Gap(context.Background(), gap.ID); err != nil || !ok || g.PrevBatch != "p2" || g.AfterTS != f.ts(rid, "$g3") {
		t.Fatalf("gap after page one = %+v %v %v", g, ok, err)
	}
	if f.m.DisplayName(rid, bob) != "Bob" {
		t.Fatal("page state names senders")
	}

	// Page two reaches $o2, stored before the gap: complete.
	_, err = f.m.FillGap(context.Background(), rid, gap.ID, []*event.Event{
		txt(t, "$g2", bob, 7_000, "g2"),
		txt(t, "$g1", bob, 3_000, "g1"),
		txt(t, "$o2", alice, 2000, "o2"),
		txt(t, "$o1", alice, 1000, "o1"),
	}, nil, "p3", false)
	if err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$o1", "$o2", "$g1", "$g2", "$g3", "$g4", "$n1", "$n2")
	if gaps, err := f.st.Gaps(context.Background(), rid); err != nil || len(gaps) != 0 {
		t.Fatalf("gaps after the last page = %+v %v", gaps, err)
	}
	if _, err := f.m.FillGap(context.Background(), rid, gap.ID, nil, nil, "", false); err == nil {
		t.Fatal("FillGap of a removed gap succeeded")
	}
}

func TestFillGapEndsWithoutNextBatch(t *testing.T) {
	f := newFixture(t)
	const rid = id.RoomID("!r:x")
	resp := newSync()
	jr := join(resp, rid)
	jr.Timeline = mautrix.SyncTimeline{Limited: true, PrevBatch: "p1"}
	jr.Timeline.Events = []*event.Event{txt(t, "$n1", alice, 10_000, "n1")}
	f.apply(resp)
	gaps, _ := f.st.Gaps(context.Background(), rid)
	gapID := gaps[0].ID
	if _, err := f.m.FillGap(context.Background(), "!other:x", gapID, nil, nil, "", true); err == nil {
		t.Fatal("FillGap of another room's gap succeeded")
	}
	if _, err := f.m.FillGap(context.Background(), rid, gapID,
		[]*event.Event{txt(t, "$first", alice, 9_000, "first")}, nil, "", false); err != nil {
		t.Fatal(err)
	}
	f.inOrder(rid, "$first", "$n1")
	if gaps, _ := f.st.Gaps(context.Background(), rid); len(gaps) != 0 {
		t.Fatalf("gaps = %+v", gaps)
	}
}

// TestRestartKeepsNameInputs: what a restarted model computes names from
// is what the old one had, so later changes rename rooms the same way.
func TestRestartKeepsNameInputs(t *testing.T) {
	f := newFixture(t)
	resp := newSync()
	// A room named "Bob", which is also what its hero would make it.
	named := join(resp, "!named:x")
	named.State.Events = []*event.Event{memberEv(t, bob, "Bob"),
		state(t, "m.room.name", "", bob, map[string]any{"name": "Bob"})}
	named.Summary = mautrix.LazyLoadSummary{Heroes: []id.UserID{bob}, JoinedMemberCount: intp(2)}
	// An unnamed room whose heroes come in the server's order, with more
	// members than heroes.
	heroes := join(resp, "!heroes:x")
	heroes.State.Events = []*event.Event{memberEv(t, carol, "Carol"), memberEv(t, bob, "Bob")}
	heroes.Summary = mautrix.LazyLoadSummary{Heroes: []id.UserID{carol, bob}, JoinedMemberCount: intp(5)}
	f.apply(resp)
	if r, _ := f.m.RoomByID("!heroes:x"); r.Name != "Carol, Bob and 2 others" {
		t.Fatalf("heroes room = %q", r.Name)
	}

	f.m = f.reopen()
	resp = newSync()
	join(resp, "!named:x").Timeline.Events = []*event.Event{memberEv(t, bob, "Robert")}
	join(resp, "!heroes:x").Timeline.Events = []*event.Event{memberEv(t, carol, "Caroline")}
	f.apply(resp)
	if r, _ := f.m.RoomByID("!named:x"); r.Name != "Bob" {
		t.Fatalf("named room after its hero's rename = %q, want its m.room.name", r.Name)
	}
	if r, _ := f.m.RoomByID("!heroes:x"); r.Name != "Caroline, Bob and 2 others" {
		t.Fatalf("heroes room after a rename = %q", r.Name)
	}
}

// TestLegacyRowsLoad: rows written before the name inputs were stored
// (schema v1) load with what the model can restore, and get their inputs
// stored the next time a batch touches them.
func TestLegacyRowsLoad(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	for _, r := range []store.Room{
		{ID: "!g:x", Name: "Group", Kind: store.RoomGroupDM, JoinState: store.Joined},
		{ID: "!c:x", Name: "chan", Kind: store.RoomChannel, JoinState: store.Joined},
	} {
		if err := f.st.PutRoom(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	f.m = f.reopen()
	if r, _ := f.m.RoomByID("!g:x"); r.Kind != store.RoomGroupDM || r.Name != "Group" {
		t.Fatalf("legacy group = %+v", r)
	}
	if r, _ := f.m.RoomByID("!c:x"); r.Kind != store.RoomChannel || r.Name != "chan" {
		t.Fatalf("legacy channel = %+v", r)
	}
	f.live("!c:x", txt(t, "$1", alice, 1000, "hi"))
	row, _, _ := f.st.GetRoom(ctx, "!c:x")
	if !row.InputsSaved || row.JoinRule != "public" || !row.HasNameEvent || row.NameEvent != "chan" || row.Kind != store.RoomChannel {
		t.Fatalf("touched legacy row = %+v", row)
	}
}
