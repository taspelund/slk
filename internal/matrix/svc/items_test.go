package svc

import (
	"context"
	"reflect"
	"testing"

	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/matrix/media"
)

// page returns the backend's newest page of general as items.
func (h *harness) page() []core.MessageItem {
	h.t.Helper()
	items, err := h.b.page(context.Background(), general, "")
	if err != nil {
		h.t.Fatalf("page: %v", err)
	}
	return items
}

func byTS(items []core.MessageItem, ts string) (core.MessageItem, bool) {
	for _, it := range items {
		if it.TS == ts {
			return it, true
		}
	}
	return core.MessageItem{}, false
}

func TestItemsRenderEachKindOfMessage(t *testing.T) {
	h := newHarness(t, nil)
	res := h.res()

	_, plainTS := h.text(alice, "hello <world>")
	html := h.ev(bob, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "**bold**", "format": "org.matrix.custom.html", "formatted_body": "<strong>bold</strong>",
	})
	htmlTS := h.live(html)
	emote := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.emote", "body": "waves"})
	emoteTS := h.live(emote)
	notice := h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.notice", "body": "build passed"})
	noticeTS := h.live(notice)
	edited, editedTS := h.text(alice, "teh typo")
	h.live(h.ev(alice, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "* the typo",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "the typo"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": edited.ID},
	}))
	// An edit by someone else is ignored.
	h.live(h.ev(bob, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "* hijacked",
		"m.new_content": map[string]any{"msgtype": "m.text", "body": "hijacked"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": edited.ID},
	}))
	redacted, redactedTS := h.text(bob, "oops")
	h.redact(redacted.ID)
	enc := h.ev(alice, "m.room.encrypted", map[string]any{"algorithm": "m.megolm.v1.aes-sha2", "ciphertext": "xyz"})
	encTS := h.live(enc)
	reply := h.ev(bob, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "> <@alice:example.org> hello\n\nhi back",
		"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$ev001"}},
	})
	replyTS := h.live(reply)
	file := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.file", "body": "report.pdf", "filename": "report.pdf", "url": "mxc://x/y"})
	fileTS := h.live(file)
	image := h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.image", "body": "look at this", "filename": "cat.png", "url": "mxc://x/z"})
	imageTS := h.live(image)

	root, rootTS := h.text(alice, "thread root")
	h.live(h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "r1", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}}))
	h.live(h.ev(self, "m.room.message", map[string]any{"msgtype": "m.text", "body": "r2", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}}))
	h.live(h.ev(bob, "m.room.message", map[string]any{"msgtype": "m.text", "body": "r3", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": root.ID}}))

	deadRoot, deadRootTS := h.text(bob, "deleted root")
	h.live(h.ev(alice, "m.room.message", map[string]any{"msgtype": "m.text", "body": "orphan", "m.relates_to": map[string]any{"rel_type": "m.thread", "event_id": deadRoot.ID}}))
	h.redact(deadRoot.ID)

	reacted, reactedTS := h.text(bob, "react to me")
	react := func(sender id.UserID, key string) id.EventID {
		e := h.ev(sender, "m.reaction", map[string]any{
			"m.relates_to": map[string]any{"rel_type": "m.annotation", "event_id": reacted.ID, "key": key},
		})
		h.live(e)
		return e.ID
	}
	react(alice, "👍")
	react(self, "👍")
	react(self, "👍") // a duplicate counts once
	react(bob, "❤️")
	react(alice, "👍🏽")
	gone := react(bob, "lol")
	h.redact(gone)

	items := h.page()
	get := func(ts string) core.MessageItem {
		t.Helper()
		it, ok := byTS(items, ts)
		if !ok {
			t.Fatalf("no item %s in %+v", ts, items)
		}
		return it
	}
	aliceAlias, bobAlias, selfAlias := res.UserAlias(alice), res.UserAlias(bob), res.UserAlias(self)

	if it := get(plainTS); it.Text != "hello &lt;world&gt;" || it.UserID != aliceAlias || it.UserName != "Alice Liddell" || it.Subtype != "" {
		t.Errorf("plain = %+v", it)
	}
	if it := get(plainTS); it.Timestamp != h.b.formatTS(plainTS) || it.Timestamp == "" {
		t.Errorf("plain timestamp = %q", it.Timestamp)
	}
	if it := get(htmlTS); it.Text != "*bold*" {
		t.Errorf("html text = %q", it.Text)
	}
	if it := get(emoteTS); it.Subtype != "me_message" || it.Text != "waves" {
		t.Errorf("emote = %+v", it)
	}
	if it := get(noticeTS); it.Subtype != "" || it.Text != "build passed" {
		t.Errorf("notice = %+v", it)
	}
	if it := get(editedTS); !it.IsEdited || it.Text != "the typo" {
		t.Errorf("edited = %+v", it)
	}
	if _, ok := byTS(items, redactedTS); ok {
		t.Errorf("redacted message shown")
	}
	if it := get(encTS); it.Text != undecryptableText || it.UserID != aliceAlias {
		t.Errorf("encrypted = %+v", it)
	}
	if it, want := get(replyTS), "> <@"+aliceAlias+">: hello &lt;world&gt;\nhi back"; it.Text != want {
		t.Errorf("reply text = %q, want %q", it.Text, want)
	}
	// A file shows as its Attachment, with the caption (if any) as text.
	if it := get(fileTS); it.Text != "" || len(it.Attachments) != 1 || it.Attachments[0].Kind != "file" || it.Attachments[0].Name != "report.pdf" {
		t.Errorf("file = %+v", it)
	} else if want, _ := media.Attachment(general, file.ID, parseMessage(file.Content.VeryRaw)); !reflect.DeepEqual(it.Attachments[0], want) {
		t.Errorf("file attachment = %+v, want %+v", it.Attachments[0], want)
	}
	if it := get(imageTS); it.Text != "look at this" || len(it.Attachments) != 1 || it.Attachments[0].Kind != "image" || it.Attachments[0].Name != "cat.png" {
		t.Errorf("image = %+v", it)
	}
	if it := get(rootTS); it.ThreadTS != rootTS || it.ReplyCount != 3 || !reflect.DeepEqual(it.ReplyUsers, []string{bobAlias, selfAlias}) {
		t.Errorf("thread root = %+v", it)
	}
	for _, it := range items {
		if it.Text == "r1" || it.Text == "r2" || it.Text == "orphan" {
			t.Errorf("thread reply %q in the channel page", it.Text)
		}
		if it.Text == "* the typo" || it.Text == "* hijacked" {
			t.Errorf("edit %q shown as a row", it.Text)
		}
	}
	if it := get(deadRootTS); it.Text != deletedText || it.ReplyCount != 1 {
		t.Errorf("redacted root with replies = %+v", it)
	}
	want := []core.ReactionItem{
		{Emoji: "+1", Count: 2, HasReacted: true, UserIDs: []string{aliceAlias, selfAlias}},
		{Emoji: "heart", Count: 1, UserIDs: []string{bobAlias}},
		{Emoji: "+1::skin-tone-4", Count: 1, UserIDs: []string{aliceAlias}},
	}
	if it := get(reactedTS); !reflect.DeepEqual(it.Reactions, want) {
		t.Errorf("reactions = %+v, want %+v", it.Reactions, want)
	}
	for i := 1; i < len(items); i++ {
		if items[i-1].TS >= items[i].TS {
			t.Fatalf("items out of order at %d: %s >= %s", i, items[i-1].TS, items[i].TS)
		}
	}
}

func TestReactionName(t *testing.T) {
	for key, want := range map[string]string{
		"👍":   "+1",
		"❤️":  "heart",
		"❤":   "heart",
		"👍🏿":  "+1::skin-tone-6",
		"lol": "lol",
		"🎉":   "tada",
	} {
		if got := reactionName(key); got != want {
			t.Errorf("reactionName(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestReplyToAMessageNotStoredSaysSo(t *testing.T) {
	h := newHarness(t, nil)
	reply := h.ev(bob, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "agreed",
		"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": "$elsewhere"}},
	})
	ts := h.live(reply)
	it, ok := byTS(h.page(), ts)
	if !ok || it.Text != "> "+unknownReplyText+"\nagreed" {
		t.Fatalf("reply = %+v", it)
	}
}

func TestMediaItemsTakeTheirFileFromTheNewestEdit(t *testing.T) {
	h := newHarness(t, nil)
	orig := h.ev(alice, "m.room.message", map[string]any{
		"msgtype": "m.image", "body": "a.png", "url": "mxc://x/a",
		"info": map[string]any{"mimetype": "image/png", "w": 100, "h": 50, "size": 7},
	})
	ts := h.live(orig)
	it, _ := byTS(h.page(), ts)
	if len(it.Attachments) != 1 || it.Text != "" || it.Attachments[0].Size != 7 {
		t.Fatalf("image = %+v", it)
	}
	if r, ok := media.ParseURL(it.Attachments[0].DownloadURL); !ok || r.Event != orig.ID || r.Room != general {
		t.Errorf("DownloadURL = %q", it.Attachments[0].DownloadURL)
	}

	// An edit that replaces the file (and adds a caption): the item
	// reads the edit's content, so its URLs name the edit.
	edit := h.ev(alice, "m.room.message", map[string]any{
		"msgtype": "m.image", "body": "* better", "filename": "b.png", "url": "mxc://x/b",
		"m.new_content": map[string]any{"msgtype": "m.image", "body": "better", "filename": "b.png", "url": "mxc://x/b"},
		"m.relates_to":  map[string]any{"rel_type": "m.replace", "event_id": orig.ID},
	})
	h.live(edit)
	it, _ = byTS(h.page(), ts)
	if !it.IsEdited || it.Text != "better" || len(it.Attachments) != 1 || it.Attachments[0].Name != "b.png" {
		t.Fatalf("edited image = %+v", it)
	}
	if r, _ := media.ParseURL(it.Attachments[0].DownloadURL); r.Event != edit.ID {
		t.Errorf("edited DownloadURL names %s, want the edit %s", r.Event, edit.ID)
	}
	c, err := h.b.mediaContent(context.Background(), general, edit.ID)
	if err != nil || c.FileName != "b.png" || c.URL != "mxc://x/b" {
		t.Errorf("mediaContent(edit) = %+v, %v", c, err)
	}

	// A reply quoting it still names the file.
	reply := h.ev(bob, "m.room.message", map[string]any{
		"msgtype": "m.text", "body": "nice",
		"m.relates_to": map[string]any{"m.in_reply_to": map[string]any{"event_id": orig.ID}},
	})
	rts := h.live(reply)
	if it, _ := byTS(h.page(), rts); it.Text != "> <@"+h.res().UserAlias(alice)+">: a.png\nnice" {
		t.Errorf("reply = %q", it.Text)
	}

	h.redact(orig.ID)
	if _, err := h.b.mediaContent(context.Background(), general, orig.ID); err == nil {
		t.Error("mediaContent of a redacted message worked")
	}
}
