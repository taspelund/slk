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

// ToMrkdwn returns the slk text of a Matrix message.
func ToMrkdwn(content *event.MessageEventContent, r Resolver) string {
	panic("mxfmt: ToMrkdwn not implemented (WP-Format)")
}

// FromCompose returns the Matrix message for text from slk's composer.
func FromCompose(text string, r Resolver) *event.MessageEventContent {
	panic("mxfmt: FromCompose not implemented (WP-Format)")
}
