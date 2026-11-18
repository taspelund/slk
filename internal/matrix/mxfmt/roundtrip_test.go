package mxfmt

import (
	"testing"

	"github.com/gammons/slk/internal/slack/mrkdwn"
	"github.com/gammons/slk/internal/ui/messages"
)

// TestRoundTripMatchesOptimisticCopy sends composer text through Matrix
// and back, ToMrkdwn(FromCompose(x)), and compares it with
// mrkdwn.Convert(x), the optimistic copy slk shows until the server echo
// replaces it. A difference slk draws the same is logged; one it draws
// differently flickers when the echo lands, and fails unless it is in
// visible below with the reason it is accepted.
func TestRoundTripMatchesOptimisticCopy(t *testing.T) {
	corpus := []string{
		"hello",
		"hi <@UALICE0001>, and <@UBOB000002>",
		"<@UNOBODY99> is not anyone",
		"see <#CGENERAL01|general>",
		"see <#CZZZ|zzz>",
		"<!here> lunch",
		"<!channel> lunch",
		"<!everyone> lunch",
		"<!subteam^S0123|@eng> look",
		"**bold** *b2* _it_ ~st~ ~~st2~~ `code`",
		"**bold _both_**",
		"a < b & c > d",
		"`<@UALICE0001>` but <@UALICE0001>",
		"```\ncode <!here> <@UALICE0001>\n  indented & <b>\n```",
		"para\n\n```\ncode\n```\nafter",
		"see https://example.com/a?b=1&c=2.",
		"mail me@example.com",
		"www.example.com",
		"[docs](https://example.com/docs)",
		"<https://x.example|X> and <https://y.example>",
		"- a\n- b\n  - c\n- d",
		"1. one\n2. two",
		"> quoted *bold*\n> two\n\nafter",
		"# Heading\n\ntext",
		"one\ntwo",
		"para one\n\npara two",
		"||spoiler||",
		"above\n\n---\n\nbelow",
		"<b>raw</b> html",
		"snake_case_word",
		"it's \"quoted\"",
		"emoji :tada:",
		"| a | b |\n|---|---|\n| 1 | 2 |",
		"@room typed",
		"say \"https://example.com\"",
		"a\\*b\\*c",
		"- top\n  1. first\n  2. second\n- next",
		"> see [docs](https://example.com) & <@UALICE0001>",
		"*one*\n*two*",
		"hello **world**\nsecond line",
		"(see https://example.com/a) ftp://files.example.org/x",
		"[Q&A <stuff>](https://example.com/q?a=1&b=2)",
	}
	// visible lists the differences slk draws, each with why it stays.
	visible := map[string]string{
		"<@UNOBODY99> is not anyone": "an alias the resolver cannot map cannot be a Matrix mention, so it goes " +
			"out as text and comes back escaped; mrkdwn.Convert draws it as a mention of @UNOBODY99",
		"see <#CZZZ|zzz>": "a room the resolver cannot map goes out as the text #zzz, drawn plain, " +
			"not mention-styled",
		"<!here> lunch":              "Matrix has one room ping: @here comes back as @channel",
		"<!everyone> lunch":          "Matrix has one room ping: @everyone comes back as @channel",
		"<!subteam^S0123|@eng> look": "Matrix has no user groups: @eng goes out as text, drawn plain",
		"`<@UALICE0001>` but <@UALICE0001>": "mrkdwn.Convert leaves code unescaped, so slk draws " +
			"a wire token inside it as a mention; the Matrix copy shows the code as typed",
		"```\ncode <!here> <@UALICE0001>\n  indented & <b>\n```": "mrkdwn.Convert leaves code unescaped, so slk draws " +
			"the wire tokens inside it as mentions; the Matrix copy shows the code as typed",
		"# Heading\n\ntext":     "a Matrix heading comes back as a bold line; mrkdwn.Convert sends \"# Heading\" as typed",
		"above\n\n---\n\nbelow": "mrkdwn.Convert drops a thematic break; the Matrix copy shows it as ---",
		"[Q&A <stuff>](https://example.com/q?a=1&b=2)": "mrkdwn.Convert drops HTML-looking text from a link label; " +
			"the Matrix copy keeps it",
	}
	r := newFakeResolver()
	for _, x := range corpus {
		got := ToMrkdwn(FromCompose(x, r), r)
		want, _ := mrkdwn.Convert(x)
		if got == want {
			continue
		}
		users, chans := r.userNames(), r.channelNames()
		if messages.RenderSlackMarkdown(got, users, chans) == messages.RenderSlackMarkdown(want, users, chans) {
			t.Logf("drawn the same: %q\n  matrix:  %q\n  convert: %q", x, got, want)
			continue
		}
		if why, ok := visible[x]; ok {
			t.Logf("visible, accepted: %q\n  matrix:  %q\n  convert: %q\n  why: %s", x, got, want, why)
			continue
		}
		t.Errorf("visible difference for %q\n  matrix:  %q\n  convert: %q", x, got, want)
	}
}
