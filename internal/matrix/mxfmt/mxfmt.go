// Package mxfmt converts message bodies between Matrix and slk.
//
// Inbound, a Matrix message's formatted_body (HTML), or its plain body when
// it has none, becomes Slack mrkdwn with wire mentions: user pills become
// <@U…>, room pills <#C…|name>, @room <!channel>, links <url|label>, and
// reply fallbacks (<mx-reply>) are dropped.
//
// Outbound, the text slk's composer produces (CommonMark plus wire
// mentions, from compose.TranslateMentionsForSend) becomes a Matrix
// message with an HTML formatted_body, mention pills and m.mentions.
package mxfmt

import (
	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// Resolver maps between Matrix IDs and slk aliases while converting. The
// svc layer implements it over ident.Mapper with in-memory caches, so
// these calls do not block on I/O. UserAlias and RoomAlias mint aliases
// for IDs not seen before.
type Resolver interface {
	UserAlias(user id.UserID) string
	RoomAlias(room id.RoomID) string
	MXID(alias string) (id.UserID, bool)
	RoomID(alias string) (id.RoomID, bool)
	// DisplayName is the name to show in a pill for user.
	DisplayName(user id.UserID) string
	// RoomName is the name to show in a pill for room.
	RoomName(room id.RoomID) string
}

// ToMrkdwn returns the slk text of a Matrix message: its formatted_body
// when that is org.matrix.custom.html, else its body.
//
//	user pill (matrix.to/#/@u:s)    <@alias>
//	room pill (matrix.to/#/!r:s)    <#alias|room name>
//	room alias or event link        <url|label>, a plain link
//	"@room" (m.mentions.room, or    <!channel>
//	  no m.mentions at all)
//	<b> <strong> / <i> <em>         *x* / _x_, per line
//	<del> <s> <strike>              ~x~
//	<code> / <pre><code>            `x` / ```x``` (language dropped)
//	<blockquote>                    "> " lines (">" for a blank one)
//	<ul> / <ol>                     "• " / "N. " lines, two spaces per level
//	<a href>                        <url|label>, <url> when label == url;
//	                                only linkscheme schemes, else the label
//	<h1>-<h6>                       a *bold* line
//	<br>, <p>                       line break, blank line between blocks
//	<hr>                            ---
//	<table>                         a line per row, cells joined by " | "
//	<img data-mx-emoticon>          :shortcode:; another <img> its alt text
//	<span data-mx-spoiler>          ||text|| or ||reason|text|| (shown, not
//	                                hidden: slk cannot hide text)
//	<mx-reply>                      dropped (the reply fallback)
//	<font>, <span>, <u>, colours    dropped, text kept
//	text                            &, <, > escaped (so text that looks
//	                                like <@U1> stays text), typed URLs as <url>
//
// A plain body is escaped the same way, its typed URLs linked, and its
// "> <@user> …" reply fallback dropped when the message is a reply.
//
// Lossy: mrkdwn has no escape for * _ ~ `, so those characters in text
// may still format in slk, and _italic_ inside a word stays literal.
//
// Only the text comes back. The caller sets what the msgtype implies:
// m.emote is MessageItem.Subtype "me_message" (slk italicises it, as
// Slack's /me), and for an edit it passes m.new_content.
func ToMrkdwn(content *event.MessageEventContent, r Resolver) string {
	if content == nil {
		return ""
	}
	in := inbound{r: r, roomPing: content.Mentions == nil || content.Mentions.Room}
	if content.Format == event.FormatHTML && content.FormattedBody != "" {
		return in.html(content.FormattedBody)
	}
	body := content.Body
	if isReply(content) {
		body = event.TrimReplyFallbackText(body)
	}
	return in.plain(body)
}

// FromCompose returns the Matrix message for text from slk's composer:
// msgtype m.text, the body as typed with mentions as names, and an HTML
// formatted_body when the HTML says more than the escaped body.
//
//	<@U…> (known alias)       user pill with the display name; the user
//	                          goes in m.mentions.user_ids
//	<@U…> (unknown alias)     the text as typed
//	<#C…|name>                room pill "#room name" (unknown alias: #name)
//	<!here> <!channel>        "@room", m.mentions.room = true
//	  <!everyone>
//	<!subteam^S…|@h>          "@h" as text: Matrix has no user groups
//	<url|label>               a link
//
// The markdown is read as mrkdwn.Convert reads it (see composeMarkdown):
// *x* is bold, typed URLs are links, raw HTML is text. Tokens inside code
// stay as typed and mention nobody.
func FromCompose(text string, r Resolver) *event.MessageEventContent {
	return fromCompose(text, r)
}
