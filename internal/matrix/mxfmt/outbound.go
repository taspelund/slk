package mxfmt

import (
	"bytes"
	"html"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	gmhtml "github.com/yuin/goldmark/renderer/html"

	"github.com/gammons/slk/internal/linkscheme"
	"github.com/gammons/slk/internal/usergroups"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/format"
	"maunium.net/go/mautrix/format/mdext"
	"maunium.net/go/mautrix/id"
)

// composeMarkdown renders composer text the way mrkdwn.Convert reads it,
// so the message Matrix users see matches slk's optimistic copy:
//
//   - *x* is bold and _x_ italic (mdext.ShortEmphasis), as in Slack;
//     **x** and __x__ stay bold.
//   - ~x~ and ~~x~~ strike through; ||x|| is a spoiler (mdext.Spoiler).
//   - Typed URLs and e-mail addresses become links (Linkify, with the
//     composer's URL rule).
//   - Raw HTML is escaped, not passed through (mdext.EscapeHTML).
//   - A line break is a line break (hard wraps).
//   - No tables: mrkdwn.Convert sends their pipes as typed.
//
// format.RenderMarkdown cannot be used as is: it has no Linkify, reads *x*
// as italic and turns pipe tables into <table>.
var composeMarkdown = goldmark.New(
	goldmark.WithExtensions(
		extension.Strikethrough,
		mdext.Spoiler,
		mdext.ShortEmphasis,
		mdext.EscapeHTML,
		extension.NewLinkify(
			extension.WithLinkifyURLRegexp(typedURLRegexp),
			extension.WithLinkifyAllowedProtocols(typedURLProtocols),
		),
	),
	goldmark.WithRendererOptions(gmhtml.WithHardWraps()),
)

// Sentinels around a token's index while the text goes through goldmark,
// as in internal/slack/mrkdwn: Private Use Area runes goldmark treats as
// plain text and the URL matcher (ASCII only) never takes into a link.
const (
	sentinelStart = '\uE000'
	sentinelEnd   = '\uE001'
)

type tokenKind int

const (
	tokUser tokenKind = iota
	tokChannel
	tokBroadcast
	tokLink
)

// token is one wire-form mention or link the composer produced.
type token struct {
	kind  tokenKind
	id    string // user or channel alias, broadcast name, or link URL
	label string
	raw   string // as typed, for code and for an alias nobody knows
}

// The composer's wire forms (compose.TranslateMentionsForSend) and
// Slack's <url|label> link, tried in order at each '<'.
var tokenPatterns = []struct {
	re   *regexp.Regexp
	kind tokenKind
}{
	{regexp.MustCompile(`^<@([A-Z0-9]+)>`), tokUser},
	{regexp.MustCompile(`^<#([A-Z0-9]+)(?:\|([^>]*))?>`), tokChannel},
	{regexp.MustCompile(`^<!([a-z]+(?:\^[A-Za-z0-9]+)?)(?:\|([^>]*))?>`), tokBroadcast},
	{regexp.MustCompile(`^<((?:` + linkscheme.Alternation + `)[^|>]+)(?:\|([^>]*))?>`), tokLink},
}

// tokenize replaces each wire-form token in s with a sentinel-wrapped
// index into the returned table.
func tokenize(s string) (string, []token) {
	if strings.ContainsAny(s, string(sentinelStart)+string(sentinelEnd)) {
		s = strings.NewReplacer(string(sentinelStart), "\uFFFD", string(sentinelEnd), "\uFFFD").Replace(s)
	}
	var table []token
	var b strings.Builder
	for i := 0; i < len(s); {
		j := strings.IndexByte(s[i:], '<')
		if j < 0 {
			b.WriteString(s[i:])
			break
		}
		b.WriteString(s[i : i+j])
		i += j
		matched := false
		for _, p := range tokenPatterns {
			m := p.re.FindStringSubmatch(s[i:])
			if m == nil {
				continue
			}
			t := token{kind: p.kind, id: m[1], raw: m[0]}
			if len(m) > 2 {
				t.label = m[2]
			}
			table = append(table, t)
			b.WriteRune(sentinelStart)
			b.WriteString(strconv.Itoa(len(table) - 1))
			b.WriteRune(sentinelEnd)
			i += len(m[0])
			matched = true
			break
		}
		if !matched {
			b.WriteByte('<')
			i++
		}
	}
	return b.String(), table
}

// sentinelAt reports the token index of a sentinel starting at s[i:]
// and the offset just past it.
func sentinelAt(s string, i int) (idx, end int, ok bool) {
	r, size := utf8.DecodeRuneInString(s[i:])
	if r != sentinelStart {
		return 0, 0, false
	}
	j := i + size
	k := j
	for k < len(s) && s[k] >= '0' && s[k] <= '9' {
		k++
	}
	r, size = utf8.DecodeRuneInString(s[k:])
	if k == j || r != sentinelEnd {
		return 0, 0, false
	}
	idx, err := strconv.Atoi(s[j:k])
	if err != nil {
		return 0, 0, false
	}
	return idx, k + size, true
}

// outbound converts one composer message.
type outbound struct {
	r        Resolver
	table    []token
	inCode   []bool // per token: it landed in code, so it stays as typed
	mentions *event.Mentions
}

func fromCompose(text string, r Resolver) *event.MessageEventContent {
	src, table := tokenize(text)
	var buf bytes.Buffer
	if err := composeMarkdown.Convert([]byte(src), &buf); err != nil {
		content := format.TextToContent(text)
		return &content
	}
	o := &outbound{r: r, table: table, inCode: make([]bool, len(table)), mentions: &event.Mentions{}}
	htmlBody := o.substitute(format.UnwrapSingleParagraph(buf.String()))
	body := o.plainBody(src)
	content := &event.MessageEventContent{
		MsgType:  event.MsgText,
		Body:     body,
		Mentions: o.mentions,
	}
	if !isEscapedText(htmlBody, body) {
		content.Format = event.FormatHTML
		content.FormattedBody = htmlBody
	}
	return content
}

// isEscapedText reports whether htmlBody says nothing body does not:
// no tags but line breaks, and the same text once unescaped.
func isEscapedText(htmlBody, body string) bool {
	h := strings.ReplaceAll(htmlBody, "<br>\n", "<br/>")
	if strings.Contains(strings.ReplaceAll(h, "<br/>", ""), "<") {
		return false
	}
	return event.ReverseTextToHTML(h) == body
}

// substitute replaces the sentinels in goldmark's HTML. Raw HTML is
// escaped, so every '<' in it starts a tag goldmark wrote.
func (o *outbound) substitute(h string) string {
	var b strings.Builder
	code, link := 0, 0
	for i := 0; i < len(h); {
		if h[i] == '<' {
			end := strings.IndexByte(h[i:], '>')
			if end < 0 {
				b.WriteString(h[i:])
				break
			}
			tag := h[i : i+end+1]
			switch {
			case strings.HasPrefix(tag, "<code"):
				code++
			case tag == "</code>":
				code--
			case strings.HasPrefix(tag, "<a "), tag == "<a>":
				link++
			case tag == "</a>":
				link--
			}
			b.WriteString(tag)
			i += end + 1
			continue
		}
		if idx, end, ok := sentinelAt(h, i); ok && idx < len(o.table) {
			if code > 0 {
				o.inCode[idx] = true
			}
			b.WriteString(o.tokenHTML(o.table[idx], code > 0, link > 0))
			i = end
			continue
		}
		b.WriteByte(h[i])
		i++
	}
	return b.String()
}

// tokenHTML is a token in the formatted body: a pill, @room, a link, or
// its plain text inside another link and as typed inside code.
func (o *outbound) tokenHTML(t token, inCode, inLink bool) string {
	if inCode {
		return html.EscapeString(t.raw)
	}
	switch t.kind {
	case tokUser:
		user, ok := o.r.MXID(t.id)
		if !ok {
			return html.EscapeString(t.raw)
		}
		name := o.userName(user)
		if inLink {
			return html.EscapeString(name)
		}
		o.mentions.Add(user)
		return pill(user.URI().MatrixToURL(), name)
	case tokChannel:
		room, ok := o.r.RoomID(t.id)
		if !ok {
			return html.EscapeString(o.plainToken(t))
		}
		name := "#" + o.roomName(room, t.label)
		if inLink {
			return html.EscapeString(name)
		}
		return pill(room.URI().MatrixToURL(), name)
	case tokBroadcast:
		if isRoomPing(t.id) && !inLink {
			o.mentions.Room = true
		}
		return html.EscapeString(o.plainToken(t))
	case tokLink:
		label := t.label
		if label == "" {
			label = t.id
		}
		if inLink {
			return html.EscapeString(label)
		}
		return pill(t.id, label)
	}
	return html.EscapeString(t.raw)
}

func pill(href, text string) string {
	return `<a href="` + html.EscapeString(href) + `">` + html.EscapeString(text) + `</a>`
}

func isRoomPing(name string) bool {
	return name == "here" || name == "channel" || name == "everyone"
}

func (o *outbound) userName(user id.UserID) string {
	if name := o.r.DisplayName(user); name != "" {
		return name
	}
	return string(user)
}

func (o *outbound) roomName(room id.RoomID, label string) string {
	if name := o.r.RoomName(room); name != "" {
		return name
	}
	if label != "" {
		return label
	}
	return string(room)
}

// plainToken is a token in the plain body.
func (o *outbound) plainToken(t token) string {
	switch t.kind {
	case tokUser:
		if user, ok := o.r.MXID(t.id); ok {
			return o.userName(user)
		}
	case tokChannel:
		if room, ok := o.r.RoomID(t.id); ok {
			return "#" + o.roomName(room, t.label)
		}
		if t.label != "" {
			return "#" + t.label
		}
	case tokBroadcast:
		if isRoomPing(t.id) {
			return "@room"
		}
		// A Slack usergroup has no Matrix equivalent: its handle, as text.
		if gid, ok := strings.CutPrefix(t.id, "subteam^"); ok {
			return usergroups.Display(nil, gid, t.label)
		}
	case tokLink:
		if t.label != "" && t.label != t.id {
			return "[" + t.label + "](" + t.id + ")"
		}
		return t.id
	}
	return t.raw
}

// plainBody is the composer text with each token in its plain form, or
// as typed where it sat in code.
func (o *outbound) plainBody(src string) string {
	var b strings.Builder
	for i := 0; i < len(src); {
		if idx, end, ok := sentinelAt(src, i); ok && idx < len(o.table) {
			if o.inCode[idx] {
				b.WriteString(o.table[idx].raw)
			} else {
				b.WriteString(o.plainToken(o.table[idx]))
			}
			i = end
			continue
		}
		b.WriteByte(src[i])
		i++
	}
	return b.String()
}
