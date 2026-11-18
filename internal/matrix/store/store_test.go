package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/crypto"
	"maunium.net/go/mautrix/id"
	"maunium.net/go/mautrix/sqlstatestore"

	"github.com/gammons/slk/internal/matrix/ident"
)

// openTest opens a store in a fresh temporary directory and closes it when
// the test ends. It returns the store and its file's path.
func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "matrix.db")
	s, err := Open(context.Background(), path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// versionOf reads a dbutil version table.
func versionOf(t *testing.T, db *dbutil.Database, table string) int {
	t.Helper()
	var v int
	if err := db.QueryRow(context.Background(), "SELECT version FROM "+table).Scan(&v); err != nil {
		t.Fatalf("reading %s: %v", table, err)
	}
	return v
}

func TestOpenMigratesFromEmpty(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if got := versionOf(t, s.DB, VersionTable); got != len(upgradeTable) {
		t.Fatalf("version = %d, want %d", got, len(upgradeTable))
	}
	for _, table := range []string{
		"slkmx_sync_state", "slkmx_rooms", "slkmx_room_space_parents", "slkmx_events",
		"slkmx_ts_map", "slkmx_aliases", "slkmx_receipts", "slkmx_gaps",
		"slkmx_media_cache_index", "slkmx_events_fts",
	} {
		if ok, err := s.DB.TableExists(ctx, table); err != nil || !ok {
			t.Errorf("table %s: exists=%v err=%v", table, ok, err)
		}
	}
	for pragma, want := range map[string]string{"journal_mode": "wal", "foreign_keys": "1", "busy_timeout": "10000"} {
		var got string
		if err := s.DB.QueryRow(ctx, "PRAGMA "+pragma).Scan(&got); err != nil || got != want {
			t.Errorf("PRAGMA %s = %q (err %v), want %q", pragma, got, err, want)
		}
	}
}

func TestReopenAtCurrentVersionIsNoop(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	if err := s.SaveNextBatch(ctx, "", "s123"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutTS(ctx, "!r:x", "$e", "0000000001.000000"); err != nil {
		t.Fatal(err)
	}
	s.Close()

	s2, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s2.Close()
	if got := versionOf(t, s2.DB, VersionTable); got != len(upgradeTable) {
		t.Fatalf("version after reopen = %d, want %d", got, len(upgradeTable))
	}
	var rows int
	if err := s2.DB.QueryRow(ctx, "SELECT count(*) FROM "+VersionTable).Scan(&rows); err != nil || rows != 1 {
		t.Fatalf("version rows = %d (err %v), want 1", rows, err)
	}
	if nb, err := s2.LoadNextBatch(ctx, ""); err != nil || nb != "s123" {
		t.Fatalf("next batch after reopen = %q, %v", nb, err)
	}
	if ts, ok, err := s2.LookupTS(ctx, "!r:x", "$e"); err != nil || !ok || ts != "0000000001.000000" {
		t.Fatalf("TS after reopen = %q %v %v", ts, ok, err)
	}
}

func TestTSPersistence(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const room, other = id.RoomID("!r:x"), id.RoomID("!o:x")

	if _, _, ok, err := s.RoomBounds(ctx, room); err != nil || ok {
		t.Fatalf("RoomBounds on empty room: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LookupTS(ctx, room, "$a"); err != nil || ok {
		t.Fatalf("LookupTS missing: ok=%v err=%v", ok, err)
	}
	if _, ok, err := s.LookupEvent(ctx, room, "0000000001.000000"); err != nil || ok {
		t.Fatalf("LookupEvent missing: ok=%v err=%v", ok, err)
	}

	for ev, ts := range map[id.EventID]string{
		"$b": "0000000002.000000",
		"$a": "0000000001.000000",
		"$c": "0000000002.000001",
	} {
		if err := s.PutTS(ctx, room, ev, ts); err != nil {
			t.Fatalf("PutTS %s: %v", ev, err)
		}
	}
	if err := s.PutTS(ctx, room, "$a", "0000000001.000000"); err != nil {
		t.Fatalf("re-putting the same pair: %v", err)
	}
	if err := s.PutTS(ctx, room, "$z", "0000000001.000000"); !errors.Is(err, ident.ErrTSTaken) {
		t.Fatalf("other event at a taken TS: err = %v, want ErrTSTaken", err)
	}
	if err := s.PutTS(ctx, room, "$a", "0000000009.000000"); err == nil || errors.Is(err, ident.ErrTSTaken) {
		t.Fatalf("moving an event to another TS: err = %v, want a non-ErrTSTaken error", err)
	}
	// TS uniqueness is per room.
	if err := s.PutTS(ctx, other, "$z", "0000000001.000000"); err != nil {
		t.Fatalf("same TS in another room: %v", err)
	}

	if ts, ok, err := s.LookupTS(ctx, room, "$c"); err != nil || !ok || ts != "0000000002.000001" {
		t.Fatalf("LookupTS $c = %q %v %v", ts, ok, err)
	}
	if ev, ok, err := s.LookupEvent(ctx, room, "0000000002.000000"); err != nil || !ok || ev != "$b" {
		t.Fatalf("LookupEvent = %q %v %v", ev, ok, err)
	}
	oldest, newest, ok, err := s.RoomBounds(ctx, room)
	if err != nil || !ok || oldest != "0000000001.000000" || newest != "0000000002.000001" {
		t.Fatalf("RoomBounds = %q %q %v %v", oldest, newest, ok, err)
	}
}

func TestAliasPersistence(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	if _, ok, err := s.LookupAlias(ctx, ident.KindUser, "@a:x"); err != nil || ok {
		t.Fatalf("LookupAlias missing: ok=%v err=%v", ok, err)
	}
	if _, _, ok, err := s.LookupMatrixID(ctx, "UAAAAAAAAAA"); err != nil || ok {
		t.Fatalf("LookupMatrixID missing: ok=%v err=%v", ok, err)
	}

	if err := s.PutAlias(ctx, ident.KindUser, "@a:x", "UAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAlias(ctx, ident.KindRoom, "!r:x", "CAAAAAAAAAA"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutAlias(ctx, ident.KindUser, "@a:x", "UAAAAAAAAAA"); err != nil {
		t.Fatalf("re-putting the same pair: %v", err)
	}
	if err := s.PutAlias(ctx, ident.KindUser, "@b:x", "UAAAAAAAAAA"); !errors.Is(err, ident.ErrAliasTaken) {
		t.Fatalf("alias of another user: err = %v, want ErrAliasTaken", err)
	}
	if err := s.PutAlias(ctx, ident.KindRoom, "@a:x", "UAAAAAAAAAA"); !errors.Is(err, ident.ErrAliasTaken) {
		t.Fatalf("alias taken by another kind: err = %v, want ErrAliasTaken", err)
	}
	if err := s.PutAlias(ctx, ident.KindUser, "@a:x", "UBBBBBBBBBB"); err == nil || errors.Is(err, ident.ErrAliasTaken) {
		t.Fatalf("second alias for a user: err = %v, want a non-ErrAliasTaken error", err)
	}

	if a, ok, err := s.LookupAlias(ctx, ident.KindUser, "@a:x"); err != nil || !ok || a != "UAAAAAAAAAA" {
		t.Fatalf("LookupAlias = %q %v %v", a, ok, err)
	}
	if _, ok, err := s.LookupAlias(ctx, ident.KindRoom, "@a:x"); err != nil || ok {
		t.Fatalf("LookupAlias with the wrong kind: ok=%v err=%v", ok, err)
	}
	if m, k, ok, err := s.LookupMatrixID(ctx, "CAAAAAAAAAA"); err != nil || !ok || m != "!r:x" || k != ident.KindRoom {
		t.Fatalf("LookupMatrixID = %q %v %v %v", m, k, ok, err)
	}
}

// TestSharesDatabaseWithMautrixStores runs mautrix's crypto and state
// store migrations on the store's file, after Open and before it.
func TestSharesDatabaseWithMautrixStores(t *testing.T) {
	ctx := context.Background()
	upgradeMautrix := func(t *testing.T, db *dbutil.Database) {
		t.Helper()
		cs := crypto.NewSQLCryptoStore(db, nil, "@me:x", "DEVICE", []byte("pickle"))
		if err := cs.DB.Upgrade(ctx); err != nil {
			t.Fatalf("crypto store upgrade: %v", err)
		}
		if err := sqlstatestore.NewSQLStateStore(db, nil, false).Upgrade(ctx); err != nil {
			t.Fatalf("state store upgrade: %v", err)
		}
		// Use the crypto store, not just its schema.
		if err := cs.PutAccount(ctx, crypto.NewOlmAccount()); err != nil {
			t.Fatalf("crypto PutAccount: %v", err)
		}
	}
	check := func(t *testing.T, path string) {
		t.Helper()
		s, err := Open(ctx, path)
		if err != nil {
			t.Fatalf("reopen: %v", err)
		}
		defer s.Close()
		upgradeMautrix(t, s.DB) // a no-op now
		if v := versionOf(t, s.DB, VersionTable); v != len(upgradeTable) {
			t.Errorf("%s = %d", VersionTable, v)
		}
		if v := versionOf(t, s.DB, "crypto_version"); v == 0 {
			t.Errorf("crypto_version = 0")
		}
		if v := versionOf(t, s.DB, sqlstatestore.VersionTableName); v == 0 {
			t.Errorf("%s = 0", sqlstatestore.VersionTableName)
		}
	}

	t.Run("store first", func(t *testing.T) {
		s, path := openTest(t)
		upgradeMautrix(t, s.DB)
		s.Close()
		check(t, path)
	})
	t.Run("mautrix first", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "matrix.db")
		db, err := dbutil.NewWithDialect(dsn(path), "sqlite")
		if err != nil {
			t.Fatal(err)
		}
		upgradeMautrix(t, db)
		db.Close()
		check(t, path)
	})
}

func testEvent(room id.RoomID, ev id.EventID, ts, body string) Event {
	return Event{
		RoomID: room, ID: ev, TS: ts, Sender: "@a:x", Type: "m.room.message",
		Content: json.RawMessage(fmt.Sprintf(`{"msgtype":"m.text","body":%q}`, body)),
		Body:    body, OriginServerTS: 1000,
	}
}

// ftsIntegrity fails the test unless the full-text index matches the
// events table exactly.
func ftsIntegrity(t *testing.T, s *Store) {
	t.Helper()
	if _, err := s.DB.Exec(context.Background(),
		`INSERT INTO slkmx_events_fts (slkmx_events_fts, rank) VALUES ('integrity-check', 1)`); err != nil {
		t.Fatalf("fts integrity-check: %v", err)
	}
}

func searchIDs(t *testing.T, s *Store, room id.RoomID, q string) []id.EventID {
	t.Helper()
	evs, err := s.SearchRoom(context.Background(), room, q, 50)
	if err != nil {
		t.Fatalf("SearchRoom %q: %v", q, err)
	}
	var out []id.EventID
	for _, e := range evs {
		out = append(out, e.ID)
	}
	return out
}

func TestSearchFollowsEvents(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const room = id.RoomID("!r:x")
	for _, e := range []Event{
		testEvent(room, "$1", "0000000001.000000", "the quick brown fox"),
		testEvent(room, "$2", "0000000002.000000", "a lazy dog"),
		testEvent(room, "$3", "0000000003.000000", "Café FOX sightings"),
		testEvent("!other:x", "$4", "0000000001.000000", "fox elsewhere"),
	} {
		if err := s.PutEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	ftsIntegrity(t, s)

	if got := searchIDs(t, s, room, "fox"); fmt.Sprint(got) != "[$3 $1]" {
		t.Fatalf("search fox = %v, want [$3 $1] (newest first, this room only)", got)
	}
	if got := searchIDs(t, s, room, "cafe qui"); len(got) != 0 {
		t.Fatalf("search across events = %v, want none", got)
	}
	if got := searchIDs(t, s, room, "cafe"); fmt.Sprint(got) != "[$3]" {
		t.Fatalf("accent-insensitive search = %v", got)
	}
	if got := searchIDs(t, s, room, `fox" OR "dog`); len(got) != 0 {
		t.Fatalf("FTS operators must be literal, got %v", got)
	}
	if got := searchIDs(t, s, room, "  "); got != nil {
		t.Fatalf("empty query = %v", got)
	}

	// An edit replaces the body.
	edited := testEvent(room, "$2", "0000000002.000000", "a lazy fox")
	if err := s.PutEvent(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, room, "dog"); len(got) != 0 {
		t.Fatalf("old body still found: %v", got)
	}
	if got := searchIDs(t, s, room, "fox"); fmt.Sprint(got) != "[$3 $2 $1]" {
		t.Fatalf("after edit: %v", got)
	}

	if err := s.RedactEvent(ctx, room, "$1"); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, room, "quick"); len(got) != 0 {
		t.Fatalf("redacted event still found: %v", got)
	}
	e, ok, err := s.GetEvent(ctx, room, "$1")
	if err != nil || !ok || !e.Redacted || e.Body != "" || string(e.Content) != "{}" {
		t.Fatalf("redacted event = %+v %v %v", e, ok, err)
	}

	// A redacted event stored whole (as /sync delivers it) is not indexed.
	red := testEvent(room, "$3", "0000000003.000000", "Café FOX sightings")
	red.Redacted = true
	if err := s.PutEvent(ctx, red); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, room, "sightings"); len(got) != 0 {
		t.Fatalf("event put as redacted still found: %v", got)
	}

	if err := s.DeleteEvent(ctx, room, "$2"); err != nil {
		t.Fatal(err)
	}
	if got := searchIDs(t, s, room, "fox"); len(got) != 0 {
		t.Fatalf("deleted event still found: %v", got)
	}
	ftsIntegrity(t, s)

	if _, err := s.DB.Exec(ctx, `UPDATE slkmx_events SET redacted = 1 WHERE event_id = $1`, "$4"); err == nil {
		t.Fatal("a redacted event with a body was accepted")
	}
}

func TestEvents(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const room = id.RoomID("!r:x")
	if _, ok, err := s.GetEvent(ctx, room, "$x"); err != nil || ok {
		t.Fatalf("GetEvent missing: ok=%v err=%v", ok, err)
	}
	for i := 1; i <= 5; i++ {
		e := testEvent(room, id.EventID(fmt.Sprintf("$%d", i)), fmt.Sprintf("%010d.000000", i), "m")
		if i == 4 {
			e.ThreadRoot, e.RelatesTo, e.RelType = "$1", "$1", "m.thread"
			e.Decrypted = true
		}
		if err := s.PutEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.EventsBefore(ctx, room, "", 2)
	if err != nil || len(got) != 2 || got[0].ID != "$4" || got[1].ID != "$5" {
		t.Fatalf("newest page = %+v, %v", got, err)
	}
	got, err = s.EventsBefore(ctx, room, got[0].TS, 10)
	if err != nil || len(got) != 3 || got[0].ID != "$1" || got[2].ID != "$3" {
		t.Fatalf("older page = %+v, %v", got, err)
	}
	e, ok, err := s.GetEvent(ctx, room, "$4")
	want := testEvent(room, "$4", "0000000004.000000", "m")
	want.ThreadRoot, want.RelatesTo, want.RelType, want.Decrypted = "$1", "$1", "m.thread", true
	if err != nil || !ok || fmt.Sprint(e) != fmt.Sprint(want) {
		t.Fatalf("GetEvent = %+v %v %v, want %+v", e, ok, err, want)
	}
	// Two events cannot share a TS in a room.
	if err := s.PutEvent(ctx, testEvent(room, "$9", "0000000001.000000", "m")); err == nil {
		t.Fatal("a second event at a taken TS was accepted")
	}
}

func TestRooms(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.GetRoom(ctx, "!a:x"); err != nil || ok {
		t.Fatalf("GetRoom missing: ok=%v err=%v", ok, err)
	}
	a := Room{ID: "!a:x", Name: "a", Kind: RoomChannel, JoinState: Joined, LastEventTS: "0000000001.000000", Encrypted: true}
	b := Room{ID: "!b:x", Kind: RoomDM, JoinState: Invited, DMUserID: "@b:x", LastEventTS: "0000000002.000000", HighlightCount: 3, MarkedUnread: true}
	for _, r := range []Room{a, b} {
		if err := s.PutRoom(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	a.Name, a.Topic = "renamed", "t"
	if err := s.PutRoom(ctx, a); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.GetRoom(ctx, "!a:x")
	if err != nil || !ok || got.Name != "renamed" || got.Topic != "t" || !got.Encrypted || got.UpdatedAt.IsZero() {
		t.Fatalf("GetRoom = %+v %v %v", got, ok, err)
	}
	list, err := s.ListRooms(ctx)
	if err != nil || len(list) != 2 || list[0].ID != "!b:x" || list[0].HighlightCount != 3 || !list[0].MarkedUnread {
		t.Fatalf("ListRooms = %+v, %v", list, err)
	}
	if err := s.PutRoom(ctx, Room{ID: "!c:x", Kind: "nonsense", JoinState: Joined}); err == nil {
		t.Fatal("unknown room kind accepted")
	}
}

func TestSyncReceiptsGaps(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()

	if nb, err := s.LoadNextBatch(ctx, ""); err != nil || nb != "" {
		t.Fatalf("initial next batch = %q, %v", nb, err)
	}
	if err := s.SaveFilterID(ctx, "", "f1"); err != nil {
		t.Fatal(err)
	}
	if f, err := s.LoadFilterID(ctx, ""); err != nil || f != "f1" {
		t.Fatalf("filter = %q, %v", f, err)
	}

	r := Receipt{RoomID: "!r:x", ThreadID: "main", UserID: "@a:x", EventID: "$1", TS: "0000000001.000000"}
	if err := s.PutReceipt(ctx, r); err != nil {
		t.Fatal(err)
	}
	r.EventID, r.TS = "$2", "0000000002.000000"
	if err := s.PutReceipt(ctx, r); err != nil {
		t.Fatal(err)
	}
	if got, ok, err := s.GetReceipt(ctx, "!r:x", "main", "@a:x"); err != nil || !ok || got != r {
		t.Fatalf("GetReceipt = %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.GetReceipt(ctx, "!r:x", "", "@a:x"); err != nil || ok {
		t.Fatalf("unthreaded receipt: ok=%v err=%v", ok, err)
	}

	g2, err := s.AddGap(ctx, "!r:x", "p2", "0000000005.000000")
	if err != nil {
		t.Fatal(err)
	}
	g1, err := s.AddGap(ctx, "!r:x", "p1", "0000000003.000000")
	if err != nil {
		t.Fatal(err)
	}
	gaps, err := s.Gaps(ctx, "!r:x")
	if err != nil || len(gaps) != 2 || gaps[0].ID != g1 || gaps[0].PrevBatch != "p1" || gaps[1].ID != g2 {
		t.Fatalf("Gaps = %+v, %v", gaps, err)
	}
	if err := s.RemoveGap(ctx, g1); err != nil {
		t.Fatal(err)
	}
	if gaps, err := s.Gaps(ctx, "!r:x"); err != nil || len(gaps) != 1 || gaps[0].ID != g2 {
		t.Fatalf("Gaps after remove = %+v, %v", gaps, err)
	}
}

// TestConcurrentWriters has several goroutines run read-then-write
// transactions (the shape ident's TS allocation has) and plain writes at
// once. None may fail with SQLITE_BUSY.
func TestConcurrentWriters(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const workers, perWorker = 8, 40
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			room := id.RoomID(fmt.Sprintf("!r%d:x", w%2))
			for i := range perWorker {
				ev := id.EventID(fmt.Sprintf("$w%di%d", w, i))
				err := s.DB.DoTxn(ctx, nil, func(ctx context.Context) error {
					_, newest, _, err := s.RoomBounds(ctx, room)
					if err != nil {
						return err
					}
					ts := nextTS(newest)
					if err := s.PutTS(ctx, room, ev, ts); err != nil {
						return err
					}
					return s.PutEvent(ctx, testEvent(room, ev, ts, "concurrent words"))
				})
				if err == nil {
					err = s.SaveNextBatch(ctx, "", string(ev))
				}
				if err != nil {
					errs <- fmt.Errorf("worker %d item %d: %w", w, i, err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	var n int
	if err := s.DB.QueryRow(ctx, "SELECT count(*) FROM slkmx_ts_map").Scan(&n); err != nil || n != workers*perWorker {
		t.Fatalf("ts rows = %d (err %v), want %d", n, err, workers*perWorker)
	}
	ftsIntegrity(t, s)
}

// nextTS is a stand-in for ident's live allocation: one microsecond after
// newest.
func nextTS(newest string) string {
	var sec, us int64
	fmt.Sscanf(newest, "%d.%d", &sec, &us)
	us++
	return fmt.Sprintf("%010d.%06d", sec+us/1e6, us%1e6)
}
