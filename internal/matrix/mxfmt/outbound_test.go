package mxfmt

import (
	"slices"
	"testing"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

const (
	alicePill = `<a href="https://matrix.to/#/@alice:example.org">Alice</a>`
	alice     = id.UserID("@alice:example.org")
)

func TestFromCompose(t *testing.T) {
	tests := []struct {
		name  string
		text  string
		body  string
		html  string // "" means no formatted_body
		users []id.UserID
		room  bool
	}{
		{
			name:  "user mention",
			text:  "hi <@UALICE0001>",
			body:  "hi Alice",
			html:  "hi " + alicePill,
			users: []id.UserID{alice},
		},
		{
			name:  "display name is escaped",
			text:  "<@UBOB000002> <@UALICE0001> <@UBOB000002>",
			body:  "Bob <Smith> Alice Bob <Smith>",
			html:  `<a href="https://matrix.to/#/@bob:example.org">Bob &lt;Smith&gt;</a> ` + alicePill + ` <a href="https://matrix.to/#/@bob:example.org">Bob &lt;Smith&gt;</a>`,
			users: []id.UserID{"@bob:example.org", alice},
		},
		{
			name: "unknown user alias stays as typed",
			text: "ping <@UNOBODY99>",
			body: "ping <@UNOBODY99>",
		},
		{
			name: "channel mention",
			text: "see <#CGENERAL01|general>",
			body: "see #general",
			// mautrix's MatrixToURL path-escapes the "!".
			html: `see <a href="https://matrix.to/#/%21general:example.org">#general</a>`,
		},
		{
			name: "unknown channel alias is its name as text",
			text: "see <#CZZZ|zzz> and <#CYYY>",
			body: "see #zzz and <#CYYY>",
		},
		{
			name: "here",
			text: "<!here> lunch",
			body: "@room lunch",
			room: true,
		},
		{
			name: "channel",
			text: "<!channel> lunch",
			body: "@room lunch",
			room: true,
		},
		{
			name: "everyone",
			text: "*<!everyone>* lunch",
			body: "*@room* lunch",
			html: "<strong>@room</strong> lunch",
			room: true,
		},
		{
			name: "usergroup is its handle as text",
			text: "<!subteam^S0123|@eng> and <!subteam^S0456> look",
			body: "@eng and @group look",
		},
		{
			name: "markdown formatting",
			text: "**bold** *also bold* _it_ ~st~ ~~st2~~ `code`",
			body: "**bold** *also bold* _it_ ~st~ ~~st2~~ `code`",
			html: "<strong>bold</strong> <strong>also bold</strong> <em>it</em> <del>st</del> <del>st2</del> <code>code</code>",
		},
		{
			name:  "mention in a code span stays literal",
			text:  "`<@UALICE0001> <!here>` and <@UALICE0001>",
			body:  "`<@UALICE0001> <!here>` and Alice",
			html:  "<code>&lt;@UALICE0001&gt; &lt;!here&gt;</code> and " + alicePill,
			users: []id.UserID{alice},
		},
		{
			name: "mentions in a code block stay literal",
			text: "```\n<!here> <@UALICE0001> <#CGENERAL01|general>\n```",
			body: "```\n<!here> <@UALICE0001> <#CGENERAL01|general>\n```",
			html: "<pre><code>&lt;!here&gt; &lt;@UALICE0001&gt; &lt;#CGENERAL01|general&gt;\n</code></pre>",
		},
		{
			name: "typed URL is a link",
			text: "see https://example.com/a?b=1&c=2.",
			body: "see https://example.com/a?b=1&c=2.",
			html: `see <a href="https://example.com/a?b=1&amp;c=2">https://example.com/a?b=1&amp;c=2</a>.`,
		},
		{
			name: "markdown link and Slack link",
			text: "[docs](https://example.com/docs) <https://x.example|X>",
			body: "[docs](https://example.com/docs) [X](https://x.example)",
			html: `<a href="https://example.com/docs">docs</a> <a href="https://x.example">X</a>`,
		},
		{
			name: "mention inside a link label is its name",
			text: "[ask <@UALICE0001>](https://example.com)",
			body: "[ask Alice](https://example.com)",
			html: `<a href="https://example.com">ask Alice</a>`,
		},
		{
			name: "raw HTML is text",
			text: "<b>not bold</b> & <i>",
			body: "<b>not bold</b> & <i>",
		},
		{
			name: "line breaks only",
			text: "one\ntwo",
			body: "one\ntwo",
		},
		{
			name: "list and quote",
			text: "- a\n- b\n\n> q",
			body: "- a\n- b\n\n> q",
			html: "<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n<blockquote>\n<p>q</p>\n</blockquote>",
		},
		{
			name: "text with a private-use sentinel character",
			text: "a\uE0000\uE001b",
			body: "a\uFFFD0\uFFFDb",
		},
		{
			name: "empty",
			text: "",
			body: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FromCompose(tt.text, newFakeResolver())
			if got.MsgType != event.MsgText {
				t.Errorf("msgtype %q", got.MsgType)
			}
			if got.Body != tt.body {
				t.Errorf("body:\n got %q\nwant %q", got.Body, tt.body)
			}
			if got.FormattedBody != tt.html {
				t.Errorf("formatted_body:\n got %q\nwant %q", got.FormattedBody, tt.html)
			}
			wantFormat := event.Format("")
			if tt.html != "" {
				wantFormat = event.FormatHTML
			}
			if got.Format != wantFormat {
				t.Errorf("format %q, want %q", got.Format, wantFormat)
			}
			if got.Mentions == nil {
				t.Fatal("m.mentions missing; an empty one says the message pings nobody")
			}
			if !slices.Equal(got.Mentions.UserIDs, tt.users) {
				t.Errorf("m.mentions.user_ids %v, want %v", got.Mentions.UserIDs, tt.users)
			}
			if got.Mentions.Room != tt.room {
				t.Errorf("m.mentions.room %v, want %v", got.Mentions.Room, tt.room)
			}
		})
	}
}
