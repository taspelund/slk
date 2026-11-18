package mxfmt

import (
	"testing"

	"github.com/gammons/slk/internal/ui/messages"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

func htmlContent(body, formatted string) *event.MessageEventContent {
	return &event.MessageEventContent{
		MsgType:       event.MsgText,
		Body:          body,
		Format:        event.FormatHTML,
		FormattedBody: formatted,
		Mentions:      &event.Mentions{},
	}
}

func plainContent(body string) *event.MessageEventContent {
	return &event.MessageEventContent{MsgType: event.MsgText, Body: body, Mentions: &event.Mentions{}}
}

func replyTo(c *event.MessageEventContent) *event.MessageEventContent {
	c.RelatesTo = &event.RelatesTo{InReplyTo: &event.InReplyTo{EventID: "$original"}}
	return c
}

// TestToMrkdwn checks Element-shaped messages against the mrkdwn they
// become, and, as a second check, the plain text slk's own flattener
// makes of that mrkdwn (what a reader sees, formatting aside).
func TestToMrkdwn(t *testing.T) {
	tests := []struct {
		name    string
		content *event.MessageEventContent
		want    string
		flat    string
	}{
		{
			name: "inline formatting",
			content: htmlContent("**bold** *it* ~~gone~~ `x < y`",
				`<strong>bold</strong> <em>it</em> <del>gone</del> <code>x &lt; y</code>`),
			want: "*bold* _it_ ~gone~ `x &lt; y`",
			flat: "*bold* _it_ ~gone~ `x < y`",
		},
		{
			name:    "b, i and s tags, nested once",
			content: htmlContent("", `<b>bold <b>still</b></b> <i>ital</i> <s>s</s> <strong><em>both</em></strong>`),
			want:    "*bold still* _ital_ ~s~ *_both_*",
			flat:    "*bold still* _ital_ ~s~ *_both_*",
		},
		{
			name:    "spaces stay outside markers",
			content: htmlContent("", `a<strong> b </strong>c`),
			want:    "a *b* c",
			flat:    "a *b* c",
		},
		{
			name: "code block with a language",
			content: htmlContent("```go\nfunc main() {\n\tprintln(\"<3\")\n}\n```",
				"<p>look:</p>\n<pre><code class=\"language-go\">func main() {\n\tprintln(&quot;&lt;3&quot;)\n}\n</code></pre>\n<p>done</p>\n"),
			want: "look:\n\n```\nfunc main() {\n\tprintln(\"&lt;3\")\n}\n```\ndone",
			flat: "look:\n\n```\nfunc main() {\n\tprintln(\"<3\")\n}\n```\ndone",
		},
		{
			name: "nested lists",
			content: htmlContent("- one\n  - sub\n- two",
				"<ul>\n<li>one\n<ul>\n<li>sub <em>a</em></li>\n<li>sub b</li>\n</ul>\n</li>\n<li>two</li>\n</ul>\n"),
			want: "• one\n  • sub _a_\n  • sub b\n• two",
			flat: "• one\n  • sub _a_\n  • sub b\n• two",
		},
		{
			name: "ordered list with a start, nested bullets, loose items",
			content: htmlContent("",
				"<ol start=\"3\">\n<li><p>three</p>\n<ul>\n<li>x</li>\n</ul>\n</li>\n<li><p>four</p>\n<p>more</p>\n</li>\n</ol>\n"),
			want: "3. three\n  • x\n4. four\n  more",
			flat: "3. three\n  • x\n4. four\n  more",
		},
		{
			name: "blockquote then a paragraph",
			content: htmlContent("> quoted **text**\n> line two\n\nafter",
				"<blockquote>\n<p>quoted <strong>text</strong><br />\nline two</p>\n</blockquote>\n<p>after</p>\n"),
			want: "> quoted *text*\n> line two\n\nafter",
			flat: "> quoted *text*\n> line two\n\nafter",
		},
		{
			name:    "blockquote with two paragraphs",
			content: htmlContent("", "<blockquote>\n<p>one</p>\n<p>two</p>\n</blockquote>\n"),
			want:    "> one\n>\n> two",
			flat:    "> one\n>\n> two",
		},
		{
			name: "labelled and bare links",
			content: htmlContent("",
				`<a href="https://example.com/a?b=1&amp;c=2">Example &amp; co</a> and <a href="https://example.com">https://example.com</a>`),
			want: "<https://example.com/a?b=1&c=2|Example &amp; co> and <https://example.com>",
			flat: "Example & co and https://example.com",
		},
		{
			name:    "link with a scheme slk does not open keeps its text",
			content: htmlContent("", `<a href="javascript:alert(1)">click</a>`),
			want:    "click",
			flat:    "click",
		},
		{
			name:    "user pill",
			content: htmlContent("Hi Alice!", `Hi <a href="https://matrix.to/#/@alice:example.org">Alice</a>!`),
			want:    "Hi <@UALICE0001>!",
			flat:    "Hi @Alice!",
		},
		{
			name:    "percent-encoded user pill",
			content: htmlContent("Bob", `<a href="https://matrix.to/#/%40bob%3Aexample.org">Bob</a>`),
			want:    "<@UBOB000002>",
			flat:    "@Bob <Smith>",
		},
		{
			name:    "room pill, known room",
			content: htmlContent("#general", `see <a href="https://matrix.to/#/!general:example.org?via=example.org">#general</a>`),
			want:    "see <#CGENERAL01|general>",
			flat:    "see #general",
		},
		{
			name:    "room pill by ID, room without a name yet",
			content: htmlContent("", `<a href="https://matrix.to/#/!other:example.org">#other</a>`),
			want:    "<#CMINT0000B|other>",
			flat:    "#other",
		},
		{
			name:    "room alias pill: no room ID, so a link",
			content: htmlContent("", `<a href="https://matrix.to/#/#random:example.org">#random:example.org</a>`),
			want:    "<https://matrix.to/#/#random:example.org|#random:example.org>",
			flat:    "#random:example.org",
		},
		{
			name:    "event permalink is a link",
			content: htmlContent("", `<a href="https://matrix.to/#/!general:example.org/$abc?via=example.org">this</a>`),
			want:    "<https://matrix.to/#/!general:example.org/$abc?via=example.org|this>",
			flat:    "this",
		},
		{
			name:    "matrix: URI becomes a matrix.to link",
			content: htmlContent("", `<a href="matrix:r/random:example.org">room</a>`),
			want:    "<https://matrix.to/#/%23random:example.org|room>",
			flat:    "room",
		},
		{
			name: "@room with m.mentions.room",
			content: &event.MessageEventContent{MsgType: event.MsgText, Body: "@room meeting now",
				Mentions: &event.Mentions{Room: true}},
			want: "<!channel> meeting now",
			flat: "@channel meeting now",
		},
		{
			name:    "@room in HTML from a client without m.mentions",
			content: &event.MessageEventContent{MsgType: event.MsgText, Body: "hey @room", Format: event.FormatHTML, FormattedBody: "<b>hey</b> @room"},
			want:    "*hey* <!channel>",
			flat:    "*hey* @channel",
		},
		{
			name:    "@room text that does not ping, and a user called room",
			content: plainContent("@room is a word; ask @room:example.org"),
			want:    "@room is a word; ask @room:example.org",
			flat:    "@room is a word; ask @room:example.org",
		},
		{
			name: "reply with an mx-reply fallback",
			content: replyTo(htmlContent("> <@bob:example.org> original\n\nmy *answer*",
				`<mx-reply><blockquote><a href="https://matrix.to/#/!general:example.org/$original?via=example.org">In reply to</a> <a href="https://matrix.to/#/@bob:example.org">@bob:example.org</a><br>original</blockquote></mx-reply>my <em>answer</em>`)),
			want: "my _answer_",
			flat: "my _answer_",
		},
		{
			name:    "reply, plain body fallback",
			content: replyTo(plainContent("> <@bob:example.org> original\n> second line\n\nmy answer")),
			want:    "my answer",
			flat:    "my answer",
		},
		{
			name:    "plain quote that is not a reply stays",
			content: plainContent("> <@bob:example.org> said\n\nhm"),
			want:    "&gt; &lt;@bob:example.org&gt; said\n\nhm",
			flat:    "> <@bob:example.org> said\n\nhm",
		},
		{
			name:    "emote",
			content: &event.MessageEventContent{MsgType: event.MsgEmote, Body: "waves"},
			want:    "waves",
			flat:    "waves",
		},
		{
			name:    "spoiler",
			content: htmlContent("", `the end: <span data-mx-spoiler>he <b>lives</b></span>`),
			want:    "the end: ||he *lives*||",
			flat:    "the end: ||he *lives*||",
		},
		{
			name:    "spoiler with a reason",
			content: htmlContent("", `<span data-mx-spoiler="film">twist</span>`),
			want:    "||film|twist||",
			flat:    "||film|twist||",
		},
		{
			name: "image emote",
			content: htmlContent(":partyparrot:",
				`yes <img data-mx-emoticon src="mxc://example.org/abc" alt=":partyparrot:" title=":partyparrot:" height="32" /> <img data-mx-emoticon src="mxc://example.org/def" alt="blob" />`),
			want: "yes :partyparrot: :blob:",
			flat: "yes :partyparrot: :blob:",
		},
		{
			name:    "heading",
			content: htmlContent("", "<h2>Title <em>here</em></h2>\n<p>text</p>\n"),
			want:    "*Title _here_*\n\ntext",
			flat:    "*Title _here_*\n\ntext",
		},
		{
			name:    "colours dropped",
			content: htmlContent("", `<font color="#ff0000">red</font> <span data-mx-color="#00ff00" data-mx-bg-color="#000">green</span>`),
			want:    "red green",
			flat:    "red green",
		},
		{
			name:    "line breaks",
			content: htmlContent("", "one<br>\ntwo<br /><br />three"),
			want:    "one\ntwo\n\nthree",
			flat:    "one\ntwo\n\nthree",
		},
		{
			name:    "plain body with < & >",
			content: plainContent("if a < b && c > d"),
			want:    "if a &lt; b &amp;&amp; c &gt; d",
			flat:    "if a < b && c > d",
		},
		{
			name:    "plain body that looks like a Slack mention",
			content: plainContent("typed <@U123> and <!here> and <#C1|x>"),
			want:    "typed &lt;@U123&gt; and &lt;!here&gt; and &lt;#C1|x&gt;",
			flat:    "typed <@U123> and <!here> and <#C1|x>",
		},
		{
			name:    "HTML text that looks like a Slack mention",
			content: htmlContent("", "<b>typed</b> &lt;@U123&gt;"),
			want:    "*typed* &lt;@U123&gt;",
			flat:    "*typed* <@U123>",
		},
		{
			name:    "plain body URLs",
			content: plainContent("see https://example.com/x?a=1&b=2. (or http://example.org/a_(b)) xhttp://no"),
			want:    "see <https://example.com/x?a=1&b=2>. (or <http://example.org/a_(b)>) xhttp://no",
			flat:    "see https://example.com/x?a=1&b=2. (or http://example.org/a_(b)) xhttp://no",
		},
		{
			name:    "URL in HTML text, none in code",
			content: htmlContent("", "go to https://example.com <code>https://example.org</code>"),
			want:    "go to <https://example.com> `https://example.org`",
			flat:    "go to https://example.com `https://example.org`",
		},
		{
			name:    "table",
			content: htmlContent("", "<table><thead><tr><th>a</th><th>b</th></tr></thead><tbody><tr><td>1</td><td><b>2</b></td></tr></tbody></table>"),
			want:    "a | b\n1 | *2*",
			flat:    "a | b\n1 | *2*",
		},
		{
			name:    "nil content",
			content: nil,
			want:    "",
			flat:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := newFakeResolver()
			r.UserAlias("@minted:example.org") // a third user, so minted room aliases are stable
			got := ToMrkdwn(tt.content, r)
			if got != tt.want {
				t.Errorf("ToMrkdwn:\n got %q\nwant %q", got, tt.want)
			}
			if flat := messages.FlattenMrkdwn(got, r.lookupUser, r.lookupChannel); flat != tt.flat {
				t.Errorf("FlattenMrkdwn(ToMrkdwn):\n got %q\nwant %q", flat, tt.flat)
			}
		})
	}
}

// TestToMrkdwnMentionsReachSlk checks that a pill of the user becomes
// the mention slk highlights and notifies on, and that text which only
// looks like one does not.
func TestToMrkdwnMentionsReachSlk(t *testing.T) {
	r := newFakeResolver()
	self := messages.SelfMentions{UserID: r.UserAlias("@alice:example.org")}
	pill := ToMrkdwn(htmlContent("Alice", `<a href="https://matrix.to/#/@alice:example.org">Alice</a> hi`), r)
	if !self.InText(pill) {
		t.Errorf("pill %q does not mention self", pill)
	}
	room := ToMrkdwn(&event.MessageEventContent{Body: "@room hi", Mentions: &event.Mentions{Room: true}}, r)
	if !self.InText(room) {
		t.Errorf("@room %q does not reach self", room)
	}
	fake := ToMrkdwn(plainContent("<@"+self.UserID+"> <!channel>"), r)
	if self.InText(fake) {
		t.Errorf("typed text %q mentions self", fake)
	}
}

// TestToMrkdwnUnknownUserMintsAlias checks a pill of a user the resolver
// has not seen gets a fresh alias in the shape slk's renderer accepts.
func TestToMrkdwnUnknownUserMintsAlias(t *testing.T) {
	r := newFakeResolver()
	got := ToMrkdwn(htmlContent("x", `<a href="https://matrix.to/#/@carol:example.org">Carol</a>`), r)
	want := "<@" + r.userAlias[id.UserID("@carol:example.org")] + ">"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
