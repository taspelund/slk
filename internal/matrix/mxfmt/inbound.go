package mxfmt

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/net/html"

	"github.com/gammons/slk/internal/linkscheme"

	"maunium.net/go/mautrix/event"
	"maunium.net/go/mautrix/id"
)

// inbound converts one Matrix message to mrkdwn.
type inbound struct {
	r Resolver
	// roomPing makes the word "@room" in text a <!channel> broadcast:
	// the message says it pings the room (m.mentions.room), or it
	// predates m.mentions, where "@room" in the body is what notifies.
	roomPing bool
}

// style is the inline formatting a node is already inside, so a nested
// <b><strong>x</strong></b> is marked once and text in code or a link
// label is neither linkified nor turned into a broadcast.
type style struct {
	bold, italic, strike, code, link bool
}

// slackEscaper escapes text the way Slack does, so the renderer shows
// it literally: "<@U1>" typed as text stays text.
var slackEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func (in inbound) plain(s string) string {
	return in.text(strings.ReplaceAll(s, "\r\n", "\n"))
}

// text is literal message text as mrkdwn: escaped, its typed URLs as
// <url> links, and "@room" as <!channel> when it pings.
func (in inbound) text(s string) string {
	var b strings.Builder
	last := 0
	for _, sp := range bareURLSpans(s) {
		b.WriteString(in.words(s[last:sp[0]]))
		b.WriteString("<" + s[sp[0]:sp[1]] + ">")
		last = sp[1]
	}
	b.WriteString(in.words(s[last:]))
	return b.String()
}

func (in inbound) words(s string) string {
	s = slackEscaper.Replace(s)
	if in.roomPing {
		s = replaceRoomPing(s)
	}
	return s
}

// replaceRoomPing turns each "@room" word into <!channel>. "@room:server"
// is a user ID, not the keyword.
func replaceRoomPing(s string) string {
	const kw = "@room"
	if !strings.Contains(s, kw) {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, kw)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		end := i + len(kw)
		before, _ := utf8.DecodeLastRuneInString(s[:i])
		after, size := utf8.DecodeRuneInString(s[end:])
		ok := (i == 0 || !isWordRune(before)) &&
			(end == len(s) || (!isWordRune(after) && !(after == ':' && end+size < len(s) && s[end+size] != ' ')))
		b.WriteString(s[:i])
		if ok {
			b.WriteString("<!channel>")
		} else {
			b.WriteString(kw)
		}
		s = s[end:]
	}
}

func isWordRune(r rune) bool {
	return r == '_' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func (in inbound) html(s string) string {
	doc, err := html.Parse(strings.NewReader(s))
	if err != nil {
		return in.plain(s)
	}
	body := findElement(doc, "body")
	if body == nil {
		return ""
	}
	return strings.Trim(in.blocks(body, style{}), "\n")
}

func findElement(n *html.Node, tag string) *html.Node {
	if n.Type == html.ElementNode && n.Data == tag {
		return n
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if f := findElement(c, tag); f != nil {
			return f
		}
	}
	return nil
}

func attr(n *html.Node, key string) (string, bool) {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val, true
		}
	}
	return "", false
}

func isBlock(n *html.Node) bool {
	if n.Type != html.ElementNode {
		return false
	}
	switch n.Data {
	case "p", "div", "h1", "h2", "h3", "h4", "h5", "h6", "ul", "ol", "li", "pre",
		"blockquote", "hr", "table", "mx-reply", "details", "summary", "dl", "dt", "dd",
		"section", "article", "header", "footer", "figure", "figcaption":
		return true
	}
	return false
}

// blocks converts the children of n: each run of inline nodes is a
// paragraph, and blocks are separated the way mrkdwn.Convert separates
// them (a blank line, but only a line break after a code block), so a
// message slk sent looks the same before and after the server echo.
func (in inbound) blocks(n *html.Node, st style) string {
	type part struct {
		text string
		pre  bool
	}
	var parts []part
	var para strings.Builder
	flush := func() {
		if t := tidyParagraph(para.String()); t != "" {
			parts = append(parts, part{text: t})
		}
		para.Reset()
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		if !isBlock(c) {
			para.WriteString(in.inline(c, st))
			continue
		}
		flush()
		if t, pre := in.block(c, st); t != "" {
			parts = append(parts, part{text: t, pre: pre})
		}
	}
	flush()
	var b strings.Builder
	for i, p := range parts {
		if i > 0 {
			if parts[i-1].pre {
				b.WriteString("\n")
			} else {
				b.WriteString("\n\n")
			}
		}
		b.WriteString(p.text)
	}
	return b.String()
}

// tidyParagraph collapses a paragraph's whitespace the way HTML does:
// runs of spaces become one and lines are trimmed.
func tidyParagraph(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.FieldsFunc(l, func(r rune) bool { return r == ' ' }), " ")
	}
	return strings.Trim(strings.Join(lines, "\n"), "\n")
}

// block converts one block element; pre reports a code block.
func (in inbound) block(n *html.Node, st style) (text string, pre bool) {
	switch n.Data {
	case "mx-reply":
		return "", false
	case "h1", "h2", "h3", "h4", "h5", "h6":
		if st.bold {
			return in.blocks(n, st), false
		}
		bold := st
		bold.bold = true
		return wrapLines(in.blocks(n, bold), "*"), false
	case "ul", "ol":
		return in.list(n, st, 0), false
	case "pre":
		return in.pre(n), true
	case "blockquote":
		inner := in.blocks(n, st)
		if inner == "" {
			return "", false
		}
		lines := strings.Split(inner, "\n")
		for i, l := range lines {
			if l == "" {
				lines[i] = ">"
			} else {
				lines[i] = "> " + l
			}
		}
		return strings.Join(lines, "\n"), false
	case "hr":
		return "---", false
	case "table":
		return in.table(n, st), false
	}
	return in.blocks(n, st), false
}

// list renders a list as mrkdwn.Convert does: "• " or "N. " markers,
// two spaces of indent per nesting level.
func (in inbound) list(n *html.Node, st style, level int) string {
	num := 1
	if s, ok := attr(n, "start"); ok {
		if v, err := strconv.Atoi(s); err == nil {
			num = v
		}
	}
	indent := strings.Repeat("  ", level)
	var out []string
	for li := n.FirstChild; li != nil; li = li.NextSibling {
		if li.Type != html.ElementNode || li.Data != "li" {
			continue
		}
		marker := "• "
		if n.Data == "ol" {
			marker = strconv.Itoa(num) + ". "
			num++
		}
		out = append(out, in.listItem(li, st, level, indent+marker)...)
	}
	return strings.Join(out, "\n")
}

// listItem returns an item's lines: its first text line after the
// marker, further text lines indented under it, nested lists as they
// render themselves.
func (in inbound) listItem(li *html.Node, st style, level int, marker string) []string {
	cont := strings.Repeat("  ", level+1)
	var lines []string
	first := true
	addText := func(t string) {
		if t == "" {
			return
		}
		for _, l := range strings.Split(t, "\n") {
			if first {
				lines = append(lines, marker+l)
				first = false
			} else {
				lines = append(lines, cont+l)
			}
		}
	}
	var para strings.Builder
	flush := func() {
		addText(tidyParagraph(para.String()))
		para.Reset()
	}
	for c := li.FirstChild; c != nil; c = c.NextSibling {
		switch {
		case c.Type == html.ElementNode && (c.Data == "ul" || c.Data == "ol"):
			flush()
			if first {
				lines = append(lines, strings.TrimRight(marker, " "))
				first = false
			}
			if nested := in.list(c, st, level+1); nested != "" {
				lines = append(lines, nested)
			}
		case isBlock(c):
			flush()
			t, _ := in.block(c, st)
			addText(t)
		default:
			para.WriteString(in.inline(c, st))
		}
	}
	flush()
	if first {
		lines = append(lines, strings.TrimRight(marker, " "))
	}
	return lines
}

// pre renders a code block as ```fenced``` mrkdwn. The language is
// dropped: mrkdwn has nowhere to put it.
func (in inbound) pre(n *html.Node) string {
	var b strings.Builder
	rawText(&b, n)
	code := strings.TrimSuffix(b.String(), "\n")
	return "```\n" + slackEscaper.Replace(code) + "\n```"
}

// rawText appends n's text with its whitespace kept, <br> as a line
// break.
func rawText(b *strings.Builder, n *html.Node) {
	switch {
	case n.Type == html.TextNode:
		b.WriteString(n.Data)
	case n.Type == html.ElementNode && n.Data == "br":
		b.WriteString("\n")
	}
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		rawText(b, c)
	}
}

// table renders each row as its cells joined by " | ".
func (in inbound) table(n *html.Node, st style) string {
	var rows []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			if c.Type != html.ElementNode {
				continue
			}
			if c.Data != "tr" {
				walk(c)
				continue
			}
			var cells []string
			for td := c.FirstChild; td != nil; td = td.NextSibling {
				if td.Type == html.ElementNode && (td.Data == "td" || td.Data == "th") {
					cells = append(cells, strings.ReplaceAll(tidyParagraph(in.children(td, st)), "\n", " "))
				}
			}
			rows = append(rows, strings.Join(cells, " | "))
		}
	}
	walk(n)
	return strings.Join(rows, "\n")
}

func (in inbound) children(n *html.Node, st style) string {
	var b strings.Builder
	for c := n.FirstChild; c != nil; c = c.NextSibling {
		b.WriteString(in.inline(c, st))
	}
	return b.String()
}

// collapseSpace replaces each whitespace character with a space; runs
// are collapsed per paragraph by tidyParagraph.
func collapseSpace(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\n', '\r', '\t', '\f':
			return ' '
		}
		return r
	}, s)
}

func (in inbound) inline(n *html.Node, st style) string {
	switch n.Type {
	case html.TextNode:
		t := collapseSpace(n.Data)
		if st.code || st.link {
			return slackEscaper.Replace(t)
		}
		return in.text(t)
	case html.ElementNode:
	default:
		return ""
	}
	if isBlock(n) {
		// A block where only inline content belongs (inside <b> or <a>):
		// keep its text on lines of its own.
		t, _ := in.block(n, st)
		if t == "" {
			return ""
		}
		return "\n" + t + "\n"
	}
	switch n.Data {
	case "br":
		return "\n"
	case "b", "strong":
		return in.mark(n, st, func(s *style) *bool { return &s.bold }, "*")
	case "i", "em":
		return in.mark(n, st, func(s *style) *bool { return &s.italic }, "_")
	case "s", "del", "strike":
		return in.mark(n, st, func(s *style) *bool { return &s.strike }, "~")
	case "code", "tt", "samp", "kbd":
		return in.mark(n, st, func(s *style) *bool { return &s.code }, "`")
	case "a":
		return in.link(n, st)
	case "img":
		return in.img(n)
	case "span":
		inner := in.children(n, st)
		if reason, ok := attr(n, "data-mx-spoiler"); ok {
			// slk cannot hide text, so a spoiler is shown with the
			// markers Matrix markdown writes one with.
			if reason != "" {
				return "||" + slackEscaper.Replace(reason) + "|" + inner + "||"
			}
			return "||" + inner + "||"
		}
		return inner
	case "script", "style", "head", "title":
		return ""
	}
	// font, u, ins, sup, sub, and anything unknown: the text alone.
	return in.children(n, st)
}

// mark wraps n's content in a mrkdwn marker, unless the text is already
// inside that formatting.
func (in inbound) mark(n *html.Node, st style, flag func(*style) *bool, marker string) string {
	if *flag(&st) {
		return in.children(n, st)
	}
	*flag(&st) = true
	return wrapLines(in.children(n, st), marker)
}

// wrapLines puts marker around each line's text, with the line's outer
// spaces outside it: slk's mrkdwn patterns never span a line break.
func wrapLines(s, marker string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		core := strings.Trim(l, " ")
		if core == "" {
			continue
		}
		start := strings.Index(l, core)
		lines[i] = l[:start] + marker + core + marker + l[start+len(core):]
	}
	return strings.Join(lines, "\n")
}

func (in inbound) link(n *html.Node, st style) string {
	href, _ := attr(n, "href")
	label := plainText(n)
	if href == "" {
		return in.children(n, st)
	}
	if uri, err := id.ParseMatrixURIOrMatrixToURL(href); err == nil && uri != nil {
		switch {
		case uri.Sigil1 == '@' && uri.Sigil2 == 0:
			return "<@" + in.r.UserAlias(uri.UserID()) + ">"
		case uri.Sigil1 == '!' && uri.Sigil2 == 0:
			room := uri.RoomID()
			name := in.r.RoomName(room)
			if name == "" {
				name = strings.TrimPrefix(label, "#")
			}
			if name == "" {
				name = string(room)
			}
			return "<#" + in.r.RoomAlias(room) + "|" + slackEscaper.Replace(name) + ">"
		}
		if strings.HasPrefix(href, "matrix:") {
			// slk links only the schemes in linkscheme.
			href = uri.MatrixToURL()
		}
	}
	if !linkscheme.HasPrefix(href) {
		// javascript:, relative and other targets slk does not open.
		return in.children(n, st)
	}
	return linkWire(href, label)
}

// linkWire is a link in Slack's wire form: <url> when the label is the
// URL, else <url|label>.
func linkWire(href, label string) string {
	bare := label == "" || label == href
	href = strings.NewReplacer("<", "%3C", ">", "%3E", "\n", "", " ", "%20").Replace(href)
	if bare {
		return "<" + href + ">"
	}
	return "<" + strings.ReplaceAll(href, "|", "%7C") + "|" + slackEscaper.Replace(label) + ">"
}

// plainText is n's text content, whitespace collapsed, images as their
// alt text: what a link label shows.
func plainText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(collapseSpace(n.Data))
		case n.Type == html.ElementNode && n.Data == "img":
			b.WriteString(imgText(n))
		case n.Type == html.ElementNode && n.Data == "br":
			b.WriteString(" ")
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(n)
	return strings.Join(strings.Fields(b.String()), " ")
}

func (in inbound) img(n *html.Node) string {
	return slackEscaper.Replace(imgText(n))
}

// imgText is what an inline image shows: a custom emoji (data-mx-emoticon)
// as its :shortcode:, another image as its alt or title text.
func imgText(n *html.Node) string {
	alt, _ := attr(n, "alt")
	if alt == "" {
		alt, _ = attr(n, "title")
	}
	if _, emoji := attr(n, "data-mx-emoticon"); emoji {
		name := strings.Trim(alt, ":")
		if name == "" {
			return ":emoji:"
		}
		return fmt.Sprintf(":%s:", name)
	}
	if alt == "" {
		return "[image]"
	}
	return alt
}

// isReply reports whether content carries a reply fallback to strip.
func isReply(content *event.MessageEventContent) bool {
	return content.RelatesTo.GetReplyTo() != ""
}
