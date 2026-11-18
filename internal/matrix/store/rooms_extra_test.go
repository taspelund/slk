package store

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

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
