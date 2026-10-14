package segment

import (
	"regexp"
	"strings"
	"unicode"
)

var reSpaceBeforePunct = regexp.MustCompile(`\s+([.,;:])`)

// NormalizeText cleans text for the model only (reported segment text and
// offsets are never normalised): every run of whitespace, including hard line
// wraps and exotic spaces such as U+202F, becomes one space; zero-width
// characters and soft hyphens are removed; and a space before . , ; : is
// dropped (a common leftover of stripped markup such as citations).
func NormalizeText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	space := false
	for _, r := range s {
		switch {
		case r == '\u200b' || r == '\u200c' || r == '\u200d' || r == '\u2060' || r == '\ufeff' || r == '\u00ad':
			continue
		case unicode.IsSpace(r):
			space = true
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	return reSpaceBeforePunct.ReplaceAllString(b.String(), "$1")
}
