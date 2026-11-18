package svc

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"

	"github.com/gammons/slk/internal/core"
	"github.com/gammons/slk/internal/emoji"
	"github.com/gammons/slk/internal/export"
	"github.com/gammons/slk/internal/matrix/media"
	"github.com/gammons/slk/internal/matrix/mxfmt"
	"github.com/gammons/slk/internal/matrix/store"
)

// How stored events become the UI's message rows.
//
//   - A row is an m.room.message, m.sticker or (still) m.room.encrypted
//     event that is not an edit or a reaction. Thread replies (m.thread)
//     are rows of their thread, not of the channel, as in Slack. Every
//     other event (state, redactions, edits, reactions) only changes rows.
//   - Text is mxfmt.ToMrkdwn of the newest m.replace edit by the original
//     sender (its m.new_content), else of the event; IsEdited says an
//     edit was applied.
//   - m.emote is Subtype "me_message" (slk italicises it). m.notice is an
//     ordinary message: Matrix bots post notices where Slack bots post
//     messages, and the UI has no notice style for chat messages.
//   - Media (m.image, m.file, m.audio, m.video) show their caption as
//     text ("" without one) and the file as their one Attachment
//     (media.Attachment); without a file they show the caption, then
//     the file name, as text.
//   - A reply (m.in_reply_to, not a thread's fallback) starts with a
//     quote of the first line of the message it answers:
//     "> <@author>: line", or "> _a message not loaded_" when that
//     message is not stored. mxfmt has already dropped the reply fallback.
//   - A redacted message is hidden, unless it is a thread root with
//     replies, which shows as "_This message was deleted._" so its
//     thread stays reachable (Slack's tombstone).
//   - An event still encrypted shows as "_🔒 Unable to decrypt this
//     message._" under its sender: the UI has no muted style for a chat
//     message, and a system notice subtype would drop the sender.
//   - Reactions are the non-redacted m.annotation events, one per sender
//     and key, keyed by Slack shortcode (reactionName).
//   - A root's ThreadTS is its own TS once it has replies, a reply's its
//     root's; ReplyCount and ReplyUsers count the stored replies (bundled
//     aggregations are not stored).

const (
	deletedText       = "_This message was deleted._"
	undecryptableText = "_🔒 Unable to decrypt this message._"
	unknownReplyText  = "_a message not loaded_"
)

// isRow reports whether e is a message row (of a channel or a thread).
func isRow(e store.Event) bool {
	switch e.Type {
	case event.EventMessage.Type, event.EventSticker.Type, event.EventEncrypted.Type:
	default:
		return false
	}
	switch event.RelationType(e.RelType) {
	case event.RelReplace, event.RelAnnotation:
		return false
	}
	return true
}

// inThread reports whether e is a thread reply.
func inThread(e store.Event) bool {
	return e.ThreadRoot != "" || event.RelationType(e.RelType) == event.RelThread
}

// isMainRow reports whether e is a row of the channel's own timeline.
func isMainRow(e store.Event) bool { return isRow(e) && !inThread(e) }

// threadRootOf returns the root of e's thread, "" if none.
func threadRootOf(e store.Event) id.EventID {
	if e.ThreadRoot != "" {
		return e.ThreadRoot
	}
	if event.RelationType(e.RelType) == event.RelThread {
		return e.RelatesTo
	}
	return ""
}

// related is what other stored events say about a set of rows.
type related struct {
	edits     map[id.EventID]store.Event   // newest edit by the original sender
	reactions map[id.EventID][]store.Event // in TS order
	replies   map[id.EventID][]store.Event // thread replies, in TS order
}

// relatedTo loads the edits, reactions and thread replies of rows.
func (b *Backend) relatedTo(ctx context.Context, room id.RoomID, rows []store.Event) (related, error) {
	rel := related{
		edits:     map[id.EventID]store.Event{},
		reactions: map[id.EventID][]store.Event{},
		replies:   map[id.EventID][]store.Event{},
	}
	sender := make(map[id.EventID]id.UserID, len(rows))
	targets := make([]id.EventID, 0, len(rows))
	for _, r := range rows {
		sender[r.ID] = r.Sender
		targets = append(targets, r.ID)
	}
	evs, err := b.relatingEvents(ctx, room, targets)
	if err != nil {
		return rel, err
	}
	for _, e := range evs {
		if e.Redacted {
			continue
		}
		switch {
		case event.RelationType(e.RelType) == event.RelReplace && e.Type == event.EventMessage.Type:
			if s, ok := sender[e.RelatesTo]; ok && s == e.Sender {
				rel.edits[e.RelatesTo] = e // evs are in TS order: the last is the newest
			}
		case event.RelationType(e.RelType) == event.RelAnnotation && e.Type == event.EventReaction.Type:
			rel.reactions[e.RelatesTo] = append(rel.reactions[e.RelatesTo], e)
		case isRow(e) && inThread(e):
			root := threadRootOf(e)
			if _, ok := sender[root]; ok {
				rel.replies[root] = append(rel.replies[root], e)
			}
		}
	}
	return rel, nil
}

// items turns rows (oldest first) into message items, dropping the rows
// that are not shown.
func (b *Backend) items(ctx context.Context, room id.RoomID, rows []store.Event) ([]core.MessageItem, error) {
	rel, err := b.relatedTo(ctx, room, rows)
	if err != nil {
		return nil, err
	}
	res := b.rooms.Resolver()
	out := make([]core.MessageItem, 0, len(rows))
	for _, r := range rows {
		if it, ok := b.item(ctx, room, r, rel, res); ok {
			out = append(out, it)
		}
	}
	return out, nil
}

// item is one row as the UI shows it; false when it is not shown.
func (b *Backend) item(ctx context.Context, room id.RoomID, r store.Event, rel related, res mxfmt.Resolver) (core.MessageItem, bool) {
	it := core.MessageItem{
		TS:        r.TS,
		UserID:    res.UserAlias(r.Sender),
		UserName:  b.rooms.DisplayName(room, r.Sender),
		Timestamp: b.formatTS(r.TS),
	}
	replies := rel.replies[r.ID]
	switch {
	case r.Redacted:
		if len(replies) == 0 {
			return core.MessageItem{}, false
		}
		it.Text = deletedText
	case r.Type == event.EventEncrypted.Type:
		it.Text = undecryptableText
	default:
		content := parseMessage(r.Content)
		if e, ok := rel.edits[r.ID]; ok {
			if nc := parseMessage(e.Content).NewContent; nc != nil {
				// The edit's relation stays the original's: the new
				// content does not carry m.relates_to.
				nc.RelatesTo = content.RelatesTo
				content, it.IsEdited = nc, true
			}
		}
		it.Text = messageText(content, res)
		src := r.ID // the event whose content this is
		if e, ok := rel.edits[r.ID]; ok && it.IsEdited {
			src = e.ID
		}
		if att, ok := media.Attachment(room, src, content); ok {
			it.Text = captionText(content, res)
			it.Attachments = []core.Attachment{att}
		}
		if content.MsgType == event.MsgEmote {
			it.Subtype = "me_message"
		}
		if to := content.RelatesTo.GetNonFallbackReplyTo(); to != "" {
			it.Text = b.replyQuote(ctx, room, to, res) + "\n" + it.Text
		}
	}
	if root := threadRootOf(r); root != "" {
		if ts, ok, err := b.d.IDs.TS(ctx, room, root); err == nil && ok {
			it.ThreadTS = ts
		}
	}
	if len(replies) > 0 {
		it.ThreadTS = r.TS
		it.ReplyCount = len(replies)
		seen := map[string]bool{}
		for _, rep := range replies {
			if a := res.UserAlias(rep.Sender); !seen[a] {
				seen[a] = true
				it.ReplyUsers = append(it.ReplyUsers, a)
			}
		}
	}
	it.Reactions = b.reactionItems(rel.reactions[r.ID], res)
	return it, true
}

// parseMessage decodes stored message content; a malformed one is empty.
func parseMessage(raw json.RawMessage) *event.MessageEventContent {
	var c event.MessageEventContent
	_ = json.Unmarshal(raw, &c)
	return &c
}

// messageText is a message's slk text: mxfmt's, and for media the
// caption, then the file name. A row with a file shows only the caption
// (captionText) above its Attachment; a reply's quote of it uses this.
func messageText(c *event.MessageEventContent, res mxfmt.Resolver) string {
	switch c.MsgType {
	case event.MsgImage, event.MsgFile, event.MsgAudio, event.MsgVideo:
		name := c.FileName
		if name == "" {
			return mxfmt.ToMrkdwn(c, res)
		}
		file := mxfmt.ToMrkdwn(&event.MessageEventContent{MsgType: event.MsgText, Body: name}, res)
		if c.Body == "" || c.Body == name {
			return file
		}
		return mxfmt.ToMrkdwn(c, res) + "\n" + file
	}
	return mxfmt.ToMrkdwn(c, res)
}

// captionText is a media message's caption as slk text, "" when it has
// none (its body is the file name).
func captionText(c *event.MessageEventContent, res mxfmt.Resolver) string {
	if c.GetCaption() == "" {
		return ""
	}
	return mxfmt.ToMrkdwn(c, res)
}

// replyQuote is the quote line a reply to ev starts with.
func (b *Backend) replyQuote(ctx context.Context, room id.RoomID, ev id.EventID, res mxfmt.Resolver) string {
	e, ok, err := b.d.Store.GetEvent(ctx, room, ev)
	if err != nil || !ok {
		return "> " + unknownReplyText
	}
	var line string
	switch {
	case e.Redacted:
		line = deletedText
	case e.Type == event.EventEncrypted.Type:
		line = undecryptableText
	default:
		text := messageText(parseMessage(e.Content), res)
		for l := range strings.SplitSeq(text, "\n") {
			if l = strings.TrimSpace(strings.TrimLeft(l, ">")); l != "" {
				line = l
				break
			}
		}
	}
	return "> <@" + res.UserAlias(e.Sender) + ">: " + line
}

// reactionItems aggregates reactions (in TS order) by shortcode, in order
// of each key's first use.
func (b *Backend) reactionItems(evs []store.Event, res mxfmt.Resolver) []core.ReactionItem {
	var out []core.ReactionItem
	index := map[string]int{}
	seen := map[[2]string]bool{}
	for _, e := range evs {
		var c event.ReactionEventContent
		if json.Unmarshal(e.Content, &c) != nil || c.RelatesTo.Key == "" {
			continue
		}
		name := reactionName(c.RelatesTo.Key)
		user := res.UserAlias(e.Sender)
		if seen[[2]string{name, user}] {
			continue
		}
		seen[[2]string{name, user}] = true
		i, ok := index[name]
		if !ok {
			i = len(out)
			index[name] = i
			out = append(out, core.ReactionItem{Emoji: name})
		}
		out[i].Count++
		out[i].UserIDs = append(out[i].UserIDs, user)
		if e.Sender == b.self {
			out[i].HasReacted = true
		}
	}
	return out
}

// glyphNames maps a standard emoji glyph (without variation selectors)
// to its canonical Slack shortcode. Built once, then only read.
var glyphNames = sync.OnceValue(func() map[string]string {
	codes := emoji.CodeMap()
	out := map[string]string{}
	// Sorted, so a glyph two canonical names share always maps to the
	// same one.
	for _, code := range slices.Sorted(maps.Keys(codes)) {
		key := stripVS(codes[code])
		if _, ok := out[key]; !ok {
			out[key] = emoji.CanonicalSlackName(strings.Trim(code, ":"), nil)
		}
	}
	return out
})

// stripVS drops emoji variation selectors (U+FE0E, U+FE0F), which Matrix
// clients include or leave out inconsistently.
func stripVS(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\uFE0E' || r == '\uFE0F' {
			return -1
		}
		return r
	}, s)
}

// reactionName is the slk name of a Matrix reaction key: the Slack
// shortcode of an emoji glyph (with "::skin-tone-N" for a skin tone), or
// the key itself when it is not a standard emoji (text, custom emoji).
func reactionName(key string) string {
	k := stripVS(key)
	names := glyphNames()
	if n, ok := names[k]; ok {
		return n
	}
	if r, size := utf8.DecodeLastRuneInString(k); size > 0 {
		if tone, ok := emoji.SlackSkinToneNumber(r); ok {
			if n, ok := names[k[:len(k)-size]]; ok {
				return n + "::skin-tone-" + string(rune('0'+tone))
			}
		}
	}
	return key
}

// formatTS is a TS's header time, as cmd/slk's formatTimestamp makes it.
func (b *Backend) formatTS(ts string) string {
	t, ok := export.TimeFromTS(ts)
	if !ok {
		return ts
	}
	return t.Format(b.tsFormat)
}

// eventColumns are store.Event's columns, in scanEvents' order. The
// store's own list and scanner are unexported; these queries are reads
// the store has no method for.
const eventColumns = `room_id, event_id, ts, sender, type, thread_root, relates_to_event, rel_type,
	content_json, body, decrypted, redacted, origin_server_ts`

// queryEvents runs a SELECT of eventColumns.
func (b *Backend) queryEvents(ctx context.Context, query string, args ...any) ([]store.Event, error) {
	rows, err := b.d.Store.DB.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.Event
	for rows.Next() {
		var e store.Event
		var content string
		if err := rows.Scan(&e.RoomID, &e.ID, &e.TS, &e.Sender, &e.Type, &e.ThreadRoot, &e.RelatesTo,
			&e.RelType, &content, &e.Body, &e.Decrypted, &e.Redacted, &e.OriginServerTS); err != nil {
			return nil, err
		}
		e.Content = json.RawMessage(content)
		out = append(out, e)
	}
	return out, rows.Err()
}

// relatingEvents returns room's events that relate to one of targets or
// are in a thread rooted at one, oldest first.
func (b *Backend) relatingEvents(ctx context.Context, room id.RoomID, targets []id.EventID) ([]store.Event, error) {
	const chunk = 200
	var out []store.Event
	for len(targets) > 0 {
		n := min(chunk, len(targets))
		args := []any{room}
		ph := make([]string, n)
		for i, t := range targets[:n] {
			args = append(args, t)
			ph[i] = "$" + strconv.Itoa(i+2)
		}
		in := strings.Join(ph, ", ")
		evs, err := b.queryEvents(ctx, `SELECT `+eventColumns+` FROM slkmx_events
			WHERE room_id = $1 AND (relates_to_event IN (`+in+`) OR thread_root IN (`+in+`))`, args...)
		if err != nil {
			return nil, err
		}
		out = append(out, evs...)
		targets = targets[n:]
	}
	slices.SortFunc(out, func(a, b store.Event) int { return strings.Compare(a.TS, b.TS) })
	return out, nil
}
