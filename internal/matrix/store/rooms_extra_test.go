package store

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"testing"

	"go.mau.fi/util/dbutil"
	"maunium.net/go/mautrix/id"
)

func TestSpaceParents(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, p := range []SpaceParent{
		{RoomID: "!b:x", SpaceID: "!s:x", Order: "2"},
		{RoomID: "!a:x", SpaceID: "!t:x"},
		{RoomID: "!a:x", SpaceID: "!s:x", Order: "1"},
		{RoomID: "!b:x", SpaceID: "!s:x", Order: "3"}, // replaces the order
	} {
		if err := s.PutSpaceParent(ctx, p); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteSpaceParent(ctx, "!a:x", "!t:x"); err != nil {
		t.Fatal(err)
	}
	got, err := s.SpaceParents(ctx)
	want := []SpaceParent{{RoomID: "!a:x", SpaceID: "!s:x", Order: "1"}, {RoomID: "!b:x", SpaceID: "!s:x", Order: "3"}}
	if err != nil || fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("SpaceParents = %+v, %v; want %+v", got, err, want)
	}
}

func TestUserReceipts(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	for _, r := range []Receipt{
		{RoomID: "!b:x", ThreadID: "", UserID: "@me:x", EventID: "$2", TS: "0000000002.000000"},
		{RoomID: "!a:x", ThreadID: "$root", UserID: "@me:x", EventID: "$1"},
		{RoomID: "!a:x", ThreadID: "", UserID: "@other:x", EventID: "$3"},
	} {
		if err := s.PutReceipt(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.UserReceipts(ctx, "@me:x")
	if err != nil || len(got) != 2 || got[0].RoomID != "!a:x" || got[0].ThreadID != "$root" ||
		got[1].TS != "0000000002.000000" || got[1].UserID != "@me:x" {
		t.Fatalf("UserReceipts = %+v, %v", got, err)
	}
}

func TestUnreadQueriesAndRelations(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	const room, self = id.RoomID("!r:x"), id.UserID("@me:x")
	if ts, err := s.LatestUnreadCandidate(ctx, room, self); err != nil || ts != "" {
		t.Fatalf("empty room: %q %v", ts, err)
	}
	ev := func(n int, sender id.UserID, typ, content string) Event {
		return Event{RoomID: room, ID: id.EventID(fmt.Sprintf("$%d", n)), TS: fmt.Sprintf("%010d.000000", n),
			Sender: sender, Type: typ, Content: json.RawMessage(content)}
	}
	text := `{"msgtype":"m.text","body":"x"}`
	edit := ev(5, "@a:x", "m.room.message", text)
	edit.RelatesTo, edit.RelType = "$1", "m.replace"
	thread := ev(6, "@a:x", "m.room.message", text)
	thread.RelatesTo, thread.RelType, thread.ThreadRoot = "$1", "m.thread", "$1"
	react := ev(7, "@a:x", "m.reaction", `{}`)
	react.RelatesTo, react.RelType = "$1", "m.annotation"
	redacted := ev(8, "@a:x", "m.room.message", `{}`)
	redacted.Redacted = true
	for _, e := range []Event{
		ev(1, "@a:x", "m.room.message", text),
		ev(2, "@a:x", "m.room.encrypted", `{"algorithm":"m.megolm.v1.aes-sha2"}`),
		ev(3, self, "m.room.message", text),
		ev(4, "@a:x", "m.room.message", `{"msgtype":"m.notice","body":"bot"}`),
		edit, thread, react, redacted,
		ev(9, "@a:x", "m.room.member", `{}`),
	} {
		if err := s.PutEvent(ctx, e); err != nil {
			t.Fatal(err)
		}
	}
	if ts, err := s.LatestUnreadCandidate(ctx, room, self); err != nil || ts != "0000000002.000000" {
		t.Fatalf("LatestUnreadCandidate = %q %v, want the encrypted event", ts, err)
	}
	if ts, err := s.LatestSentBy(ctx, room, self); err != nil || ts != "0000000003.000000" {
		t.Fatalf("LatestSentBy = %q %v", ts, err)
	}
	rel, err := s.Relations(ctx, room, "$1", "")
	if err != nil || len(rel) != 3 || rel[0].ID != "$5" || rel[2].ID != "$7" {
		t.Fatalf("Relations = %+v %v", rel, err)
	}
	rel, err = s.Relations(ctx, room, "$1", "m.replace")
	if err != nil || len(rel) != 1 || rel[0].ID != "$5" {
		t.Fatalf("Relations m.replace = %+v %v", rel, err)
	}
}

//go:embed testdata/v1/*.sql
var v1FS embed.FS

// TestMigrateFromV1 opens a database made with the v1 schema, holding a
// room, and checks it is upgraded with the room's inputs unsaved.
func TestMigrateFromV1(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "matrix.db")
	raw, err := sql.Open("sqlite", dsn(path))
	if err != nil {
		t.Fatal(err)
	}
	db, err := dbutil.NewWithDB(raw, "sqlite3")
	if err != nil {
		t.Fatal(err)
	}
	v1 := dbutil.BuildUpgradeTable().WithFSPath(v1FS, "testdata/v1").Finish()
	if err := db.Child(VersionTable, v1, nil).Upgrade(ctx); err != nil {
		t.Fatalf("v1 schema: %v", err)
	}
	if v := versionOf(t, db, VersionTable); v != 1 {
		t.Fatalf("v1 database at version %d", v)
	}
	if _, err := db.Exec(ctx, `INSERT INTO slkmx_rooms (room_id, name, kind, join_state, dm_user_id)
		VALUES ('!r:x', 'Alice', 'dm', 'join', '@alice:x')`); err != nil {
		t.Fatal(err)
	}
	db.Close()

	s, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open v1: %v", err)
	}
	defer s.Close()
	if v := versionOf(t, s.DB, VersionTable); v != 2 || v != len(upgradeTable) {
		t.Fatalf("version after upgrade = %d, want 2", v)
	}
	r, ok, err := s.GetRoom(ctx, "!r:x")
	if err != nil || !ok || r.Name != "Alice" || r.Kind != RoomDM || r.DMUserID != "@alice:x" ||
		r.InputsSaved || r.HasNameEvent || r.Heroes != nil || r.DirectUsers != nil || r.HaveMemberCounts {
		t.Fatalf("migrated room = %+v %v %v", r, ok, err)
	}
	// The upgraded schema is the latest revision's.
	fresh, _ := openTest(t)
	if got, want := columns(t, s.DB, "slkmx_rooms"), columns(t, fresh.DB, "slkmx_rooms"); !slices.Equal(got, want) {
		t.Fatalf("upgraded columns %v\nlatest columns %v", got, want)
	}
}

func columns(t *testing.T, db *dbutil.Database, table string) []string {
	t.Helper()
	rows, err := db.Query(context.Background(), `SELECT name, type, "notnull", coalesce(dflt_value, '') FROM pragma_table_info($1) ORDER BY name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name, typ, def string
		var notNull int
		if err := rows.Scan(&name, &typ, &notNull, &def); err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprintf("%s %s %d %s", name, typ, notNull, def))
	}
	return out
}

func TestRoomInputsRoundTrip(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	in := Room{
		ID: "!r:x", Kind: RoomGroupDM, JoinState: Joined, InputsSaved: true, HasNameEvent: true,
		Heroes: []id.UserID{"@b:x", "@a:x"}, HaveMemberCounts: true, JoinedMemberCount: 3, InvitedMemberCount: 1,
		JoinRule: "invite", DirectUsers: []id.UserID{"@a:x", "@b:x"},
	}
	if err := s.PutRoom(ctx, in); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.GetRoom(ctx, "!r:x")
	got.UpdatedAt = in.UpdatedAt
	if err != nil || !reflect.DeepEqual(got, in) {
		t.Fatalf("GetRoom = %+v, %v\nwant %+v", got, err, in)
	}
}

func TestGapUpdatesAndTSBefore(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	g, err := s.AddGap(ctx, "!r:x", "p1", "0000000005.000000")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateGap(ctx, g, "p2", "0000000004.000000"); err != nil {
		t.Fatal(err)
	}
	got, ok, err := s.Gap(ctx, g)
	if err != nil || !ok || got.RoomID != "!r:x" || got.PrevBatch != "p2" || got.AfterTS != "0000000004.000000" {
		t.Fatalf("Gap = %+v %v %v", got, ok, err)
	}
	if _, ok, err := s.Gap(ctx, g+1); err != nil || ok {
		t.Fatalf("missing gap: %v %v", ok, err)
	}
	for i, ts := range []string{"0000000001.000000", "0000000003.000000", "0000000006.000000"} {
		if err := s.PutTS(ctx, "!r:x", id.EventID(fmt.Sprintf("$%d", i)), ts); err != nil {
			t.Fatal(err)
		}
	}
	if ts, ok, err := s.TSBefore(ctx, "!r:x", "0000000004.000000"); err != nil || !ok || ts != "0000000003.000000" {
		t.Fatalf("TSBefore = %q %v %v", ts, ok, err)
	}
	if _, ok, err := s.TSBefore(ctx, "!r:x", "0000000001.000000"); err != nil || ok {
		t.Fatalf("TSBefore the oldest: %v %v", ok, err)
	}
}
