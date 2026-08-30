package api

import (
	"fmt"
	"html"
	"strings"
	"unicode/utf8"
)

// doc is one page rendered for clients that run no JS: lynx and a crawler get
// the HTML form, curl the text one, and both come from this tree.
type doc struct {
	Title    string
	Intro    string
	Nav      []cell
	Sections []section
	Footer   []cell
}

// section is a heading with any of paragraphs, label/value rows and a table.
type section struct {
	Heading string
	Notes   []string
	Defs    []def
	Table   *table
}

// def is one label/value row.
type def struct{ Key, Value string }

type table struct {
	Head []string
	Rows [][]cell
}

// cell is one field, a link where Href is set.
type cell struct{ Text, Href string }

func txt(s string) cell           { return cell{Text: s} }
func link(text, href string) cell { return cell{Text: text, Href: href} }

// fallbackID is what the shell's stylesheet hides once the bundle has run.
const fallbackID = "fallback"

// renderDocHTML is the body served inside the shell. Headings, tables and
// links and nothing else: that is the subset a text browser renders well.
func renderDocHTML(d doc) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "\n<div id=%q>\n<h1>%s</h1>\n", fallbackID, esc(d.Title))
	if len(d.Nav) > 0 {
		b.WriteString("<nav>")
		writeHTMLCells(&b, d.Nav, " | ")
		b.WriteString("</nav>\n")
	}
	if d.Intro != "" {
		fmt.Fprintf(&b, "<p>%s</p>\n", esc(d.Intro))
	}
	for _, s := range d.Sections {
		// A page whose h1 already names its one table needs no second heading.
		if s.Heading != "" {
			fmt.Fprintf(&b, "<h2>%s</h2>\n", esc(s.Heading))
		}
		for _, n := range s.Notes {
			fmt.Fprintf(&b, "<p>%s</p>\n", esc(n))
		}
		// A table rather than a dl: lynx puts the two on one line, and a dl
		// costs a line per label.
		if len(s.Defs) > 0 {
			b.WriteString("<table>\n")
			for _, kv := range s.Defs {
				fmt.Fprintf(&b, "<tr><th scope=\"row\">%s</th><td>%s</td></tr>\n",
					esc(kv.Key), esc(kv.Value))
			}
			b.WriteString("</table>\n")
		}
		if s.Table != nil {
			writeHTMLTable(&b, s.Table)
		}
	}
	if len(d.Footer) > 0 {
		b.WriteString("<hr />\n<p class=\"foot\">")
		writeHTMLCells(&b, d.Footer, " · ")
		b.WriteString("</p>\n")
	}
	b.WriteString("</div>\n")
	return []byte(b.String())
}

func esc(s string) string { return html.EscapeString(s) }

func writeHTMLCells(b *strings.Builder, cells []cell, sep string) {
	for i, c := range cells {
		if i > 0 {
			b.WriteString(sep)
		}
		if c.Href == "" {
			b.WriteString(esc(c.Text))
			continue
		}
		fmt.Fprintf(b, "<a href=%q>%s</a>", esc(c.Href), esc(c.Text))
	}
}

func writeHTMLTable(b *strings.Builder, t *table) {
	b.WriteString("<table>\n<tr>")
	for _, h := range t.Head {
		fmt.Fprintf(b, "<th>%s</th>", esc(h))
	}
	b.WriteString("</tr>\n")
	for _, row := range t.Rows {
		b.WriteString("<tr>")
		for _, c := range row {
			b.WriteString("<td>")
			writeHTMLCells(b, []cell{c}, "")
			b.WriteString("</td>")
		}
		b.WriteString("</tr>\n")
	}
	b.WriteString("</table>\n")
}

// renderDocText is the same page for a client that asked for anything but
// HTML. Columns are padded so the tables line up in a terminal.
func renderDocText(d doc) []byte {
	var b strings.Builder
	writeHeading(&b, d.Title, '=')
	if d.Intro != "" {
		b.WriteString("\n" + d.Intro + "\n")
	}
	if len(d.Nav) > 0 {
		b.WriteString("\n" + textCells(d.Nav, "  ·  ") + "\n")
	}
	for _, s := range d.Sections {
		if s.Heading != "" {
			b.WriteString("\n")
			writeHeading(&b, s.Heading, '-')
		}
		for _, n := range s.Notes {
			b.WriteString("\n" + n + "\n")
		}
		if len(s.Defs) > 0 {
			b.WriteString("\n")
			w := 0
			for _, kv := range s.Defs {
				w = max(w, width(kv.Key))
			}
			for _, kv := range s.Defs {
				fmt.Fprintf(&b, "  %s%s  %s\n", kv.Key, pad(kv.Key, w), kv.Value)
			}
		}
		if s.Table != nil {
			b.WriteString("\n")
			writeTextTable(&b, s.Table)
		}
	}
	if len(d.Footer) > 0 {
		b.WriteString("\n")
		for _, c := range d.Footer {
			b.WriteString(textCells([]cell{c}, "") + "\n")
		}
	}
	return []byte(b.String())
}

// textCells spells a link out, since a terminal cannot follow one.
func textCells(cells []cell, sep string) string {
	parts := make([]string, 0, len(cells))
	for _, c := range cells {
		switch {
		case c.Href == "":
			parts = append(parts, c.Text)
		case c.Text == "" || c.Text == c.Href:
			parts = append(parts, c.Href)
		default:
			parts = append(parts, c.Text+" ("+c.Href+")")
		}
	}
	return strings.Join(parts, sep)
}

func writeHeading(b *strings.Builder, s string, rule rune) {
	b.WriteString(s + "\n" + strings.Repeat(string(rule), width(s)) + "\n")
}

// writeTextTable pads every column but the last, which would only add trailing
// space to every line.
func writeTextTable(b *strings.Builder, t *table) {
	w := make([]int, len(t.Head))
	for i, h := range t.Head {
		w[i] = width(h)
	}
	for _, row := range t.Rows {
		for i, c := range row {
			if i < len(w) {
				w[i] = max(w[i], width(c.Text))
			}
		}
	}

	head := make([]cell, len(t.Head))
	rule := make([]cell, len(t.Head))
	for i, h := range t.Head {
		head[i] = txt(h)
		rule[i] = txt(strings.Repeat("-", w[i]))
	}
	writeTextRow(b, w, head)
	writeTextRow(b, w, rule)
	for _, row := range t.Rows {
		writeTextRow(b, w, row)
	}
}

func writeTextRow(b *strings.Builder, w []int, row []cell) {
	var line strings.Builder
	line.WriteString("  ")
	for i, c := range row {
		if i > 0 {
			line.WriteString("  ")
		}
		line.WriteString(c.Text)
		if i < len(row)-1 && i < len(w) {
			line.WriteString(pad(c.Text, w[i]))
		}
	}
	b.WriteString(strings.TrimRight(line.String(), " ") + "\n")
}

func pad(s string, w int) string { return strings.Repeat(" ", max(w-width(s), 0)) }

// width counts runes, not bytes: a hostname can be non-ASCII.
func width(s string) int { return utf8.RuneCountInString(s) }
