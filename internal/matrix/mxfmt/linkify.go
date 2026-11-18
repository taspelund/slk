package mxfmt

import (
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/gammons/slk/internal/linkscheme"
)

// typedURLRegexp is what a typed URL is, the same rule as the Slack
// backend's composer (internal/slack/mrkdwn typedURLRegexp, unexported
// there): a linkscheme scheme, then printable ASCII minus < > and |,
// minus trailing punctuation. Outbound it drives goldmark's Linkify;
// inbound it finds the URLs in Matrix text, which Matrix clients send
// unlinked and link when they draw.
var typedURLRegexp = regexp.MustCompile(`^(?:` + linkscheme.Alternation + `)[!-;=?-{}~]*[#-&()+\-/-9=@-\[\\^` + "`" + `a-{}]`)

// typedURLProtocols are the "scheme:" names Linkify may start a match
// at. mailto: is left to Linkify's email matcher.
var typedURLProtocols = func() [][]byte {
	var out [][]byte
	for _, p := range linkscheme.Prefixes {
		name, _, _ := strings.Cut(p, ":")
		if name != "mailto" {
			out = append(out, []byte(name+":"))
		}
	}
	return out
}()

// typedURLLen returns the length of the typed URL at the start of s, or
// 0 when s does not start with one. Closing parens the URL did not open
// are not part of it, as with Linkify.
func typedURLLen(s string) int {
	if !linkscheme.HasPrefix(s) {
		return 0
	}
	m := typedURLRegexp.FindString(s)
	for strings.HasSuffix(m, ")") && strings.Count(m, ")") > strings.Count(m, "(") {
		m = m[:len(m)-1]
	}
	return len(m)
}

// bareURLSpans returns the [start, end) byte ranges of the typed URLs in
// s. A URL does not start in the middle of a word ("xhttps://").
func bareURLSpans(s string) [][2]int {
	var spans [][2]int
	for i := 0; i < len(s); {
		prev, _ := utf8.DecodeLastRuneInString(s[:i])
		if i == 0 || (!unicode.IsLetter(prev) && !unicode.IsDigit(prev)) {
			if n := typedURLLen(s[i:]); n > 0 {
				spans = append(spans, [2]int{i, i + n})
				i += n
				continue
			}
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return spans
}
