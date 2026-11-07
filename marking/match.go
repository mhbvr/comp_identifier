package marking

import (
	"strings"
	"unicode"
)

// Normalize upper-cases s and removes whitespace, so that markings can be
// compared regardless of how OCR split them into words and lines.
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range s {
		if !unicode.IsSpace(r) {
			b.WriteRune(unicode.ToUpper(r))
		}
	}
	return b.String()
}

// MatchToken returns the smallest edit (Levenshtein) distance between token
// and any substring of text, both normalised with Normalize. 0 means the
// token appears verbatim. It is meant for checking an expected part number
// against a recognised marking.
func MatchToken(text, token string) int {
	t := []rune(Normalize(text))
	k := []rune(Normalize(token))
	if len(k) == 0 {
		return 0
	}
	// Approximate substring matching: the first row is all zeros so a match
	// may start anywhere in t; the answer is the minimum of the last row.
	prev := make([]int, len(t)+1)
	cur := make([]int, len(t)+1)
	for i := 1; i <= len(k); i++ {
		cur[0] = i
		for j := 1; j <= len(t); j++ {
			cost := 1
			if k[i-1] == t[j-1] {
				cost = 0
			}
			cur[j] = min(prev[j]+1, cur[j-1]+1, prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	best := prev[0]
	for _, v := range prev {
		best = min(best, v)
	}
	return best
}
