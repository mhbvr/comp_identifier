package inventory

import (
	"sort"
	"strings"

	"github.com/mhbvr/comp_identifier/marking"
)

// Match is a known component whose name or item marking was found in a
// recognised marking.
type Match struct {
	Component
	By       string // "name" or "marking" (of one of its items)
	Key      string // the name or marking text that matched
	Distance int    // edits between Key and the recognised text (0: exact)
}

// Exact reports whether the key was found without edits.
func (m Match) Exact() bool { return m.Distance == 0 }

// minKey is the shortest name or marking line looked up: shorter ones
// (logos, "TI", date codes) would match almost anything.
const minKey = 3

// tolerance is the number of OCR errors forgiven in a key of n letters:
// none for short keys, where one edit turns one part number into another.
func tolerance(n int) int {
	switch {
	case n < 6:
		return 0
	case n < 12:
		return 1
	}
	return 2
}

// Match finds the components that the recognised text (lines separated by
// "\n") belongs to, best first. A component matches when
//
//   - its name appears in the text (part numbers are printed on parts), or
//   - one of its items has the same marking, or
//   - a line of 6 or more letters of an item marking appears in the text
//     (the printed part number when the component's name is the canonical
//     one, such as LM35DZ/NOPB printed as LM35DZ),
//
// forgiving one OCR error per 6 letters (none under 6 letters). Matches
// are ordered by edits, then by the length of the matched key: a longer key
// is more specific (TLC555IP over TLC555).
func (d *DB) Match(text string) ([]Match, error) {
	if len(marking.Normalize(text)) < minKey {
		return nil, nil
	}
	comps, err := d.Components("")
	if err != nil {
		return nil, err
	}
	items, err := d.Items(0, "")
	if err != nil {
		return nil, err
	}
	best := map[int64]Match{}
	consider := func(c Component, by, key string, dist int) {
		n := len([]rune(marking.Normalize(key)))
		if n < minKey || dist > tolerance(n) {
			return
		}
		m := Match{Component: c, By: by, Key: key, Distance: dist}
		if old, ok := best[c.ID]; !ok || better(m, old) {
			best[c.ID] = m
		}
	}
	byID := map[int64]Component{}
	for _, c := range comps {
		byID[c.ID] = c
		consider(c, "name", c.Name, marking.MatchToken(text, c.Name))
	}
	for _, it := range items {
		c := byID[it.ComponentID]
		consider(c, "marking", it.Marking, marking.EditDistance(text, it.Marking))
		for _, line := range strings.Split(it.Marking, "\n") {
			if len([]rune(marking.Normalize(line))) >= 6 {
				consider(c, "marking", line, marking.MatchToken(text, line))
			}
		}
	}
	out := make([]Match, 0, len(best))
	for _, m := range best {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return better(out[i], out[j]) })
	return out, nil
}

func better(a, b Match) bool {
	if a.Distance != b.Distance {
		return a.Distance < b.Distance
	}
	la, lb := len(marking.Normalize(a.Key)), len(marking.Normalize(b.Key))
	if la != lb {
		return la > lb
	}
	return a.Name < b.Name
}
