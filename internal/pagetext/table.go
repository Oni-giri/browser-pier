package pagetext

import "strings"

// maxCell caps one table cell's text.
const maxCell = 400

// table renders a data table as a Markdown table, its first row as the
// header. A layout table (one column, or cells holding blocks such as
// lists or other tables) is rendered cell by cell as blocks instead.
func (w *writer) table(n *Node) {
	caption, rows := tableRows(n)
	cols := 0
	for _, r := range rows {
		cols = max(cols, len(r))
	}
	if cols < 2 || !dataTable(rows) {
		w.block(func() { w.kids(n) })
		return
	}
	w.flush()
	w.want(2)
	if caption != nil {
		w.block(func() { w.kids(caption) })
	}
	var lines []string
	for i, r := range rows {
		cells := make([]string, cols)
		for j, c := range r {
			cells[j] = w.cell(c)
		}
		if w.plain {
			lines = append(lines, strings.TrimSpace(strings.Join(cells, " | ")))
			continue
		}
		lines = append(lines, "| "+strings.Join(cells, " | ")+" |")
		if i == 0 {
			lines = append(lines, "|"+strings.Repeat(" --- |", cols))
		}
	}
	w.emit(lines)
	w.want(2)
}

// tableRows lists a table's caption and rows of cells, from its own
// sections only (not from tables inside it).
func tableRows(n *Node) (caption *Node, rows [][]*Node) {
	var walk func(*Node)
	walk = func(n *Node) {
		for _, k := range n.Kids {
			switch k.Tag {
			case "caption":
				caption = k
			case "thead", "tbody", "tfoot":
				walk(k)
			case "tr":
				var cells []*Node
				for _, c := range k.Kids {
					if c.Tag == "td" || c.Tag == "th" {
						cells = append(cells, c)
					}
				}
				if len(cells) > 0 {
					rows = append(rows, cells)
				}
			}
		}
	}
	walk(n)
	return caption, rows
}

// dataTable reports whether no cell holds a block that a Markdown cell
// can't: a list, a table, a heading, preformatted text or a quote.
func dataTable(rows [][]*Node) bool {
	var blocky func(*Node) bool
	blocky = func(n *Node) bool {
		for _, k := range n.Kids {
			switch k.Tag {
			case "table", "ul", "ol", "pre", "blockquote", "h1", "h2", "h3", "h4", "h5", "h6":
				return true
			}
			if k.Tag != "" && blocky(k) {
				return true
			}
		}
		return false
	}
	for _, r := range rows {
		for _, c := range r {
			if blocky(c) {
				return false
			}
		}
	}
	return true
}

// cell renders a table cell's content on one line.
func (w *writer) cell(c *Node) string {
	sub := &writer{plain: w.plain, base: w.base, limit: maxCell * 4}
	sub.kids(c)
	sub.flush()
	var parts []string
	for _, l := range sub.out {
		if l = strings.TrimSpace(l); l != "" {
			parts = append(parts, l)
		}
	}
	s := strings.Join(parts, " ")
	if r := []rune(s); len(r) > maxCell {
		s = string(r[:maxCell-1]) + "…"
	}
	if !w.plain {
		s = strings.ReplaceAll(s, "|", `\|`)
	}
	return s
}
