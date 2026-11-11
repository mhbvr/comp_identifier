package inventory

import (
	"bufio"
	"fmt"
	"io"
	"sort"
	"strings"
)

// WriteMarkdown writes components as a Markdown table of ID, name and
// description, by ID: a compact list that is easy to read or to hand to an
// AI assistant. Items, photos and datasheets are left out.
func WriteMarkdown(w io.Writer, comps []Component) error {
	comps = append([]Component(nil), comps...)
	sort.Slice(comps, func(i, j int) bool { return comps[i].ID < comps[j].ID })
	b := bufio.NewWriter(w)
	fmt.Fprintf(b, "# Components\n\n%d component%s.\n\n", len(comps), plural(len(comps)))
	if len(comps) > 0 {
		b.WriteString("| ID | Name | Description |\n|---:|---|---|\n")
		for _, c := range comps {
			fmt.Fprintf(b, "| %d | %s | %s |\n", c.ID, cell(c.Name), cell(c.Description))
		}
	}
	return b.Flush()
}

// cell makes text safe inside a Markdown table cell: one line, with pipes
// and backslashes escaped.
func cell(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	return strings.NewReplacer(`\`, `\\`, `|`, `\|`).Replace(s)
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
