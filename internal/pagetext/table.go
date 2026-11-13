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
	size, room := 0, w.room()
	for i, r := range rows {
		if room >= 0 && size > room {
			w.full = true
			break // add cuts the last line to what fits
		}
		cells := make([]string, cols)
		for j, c := range r {
			cells[j] = w.cell(c)
		}
		line := "| " + strings.Join(cells, " | ") + " |"
		if w.plain {
			line = strings.TrimSpace(strings.Join(cells, " | "))
		}
		lines = append(lines, line)
		size += len(line) + 1
		if i == 0 && !w.plain {
			lines = append(lines, "|"+strings.Repeat(" --- |", cols))
		}
	}
	full := w.full
	w.full = false // let emit write what was gathered
	w.emit(lines)
	w.full = w.full || full
	w.want(2)
}

// tableRows lists a table's caption and rows of cells, from its own
// sections only (not from tables inside it).
func tableRows(n *Node) (caption *Node, rows [][]*Node) {
	add := func(tr *Node) {
		var cells []*Node
		for _, c := range tr.Kids {
			if c.Tag == "td" || c.Tag == "th" {
				cells = append(cells, c)
			}
		}
		if len(cells) > 0 {
			rows = append(rows, cells)
		}
	}
	for _, k := range n.Kids {
		switch k.Tag {
		case "caption":
			caption = k
		case "thead", "tbody", "tfoot":
			for _, tr := range k.Kids {
				if tr.Tag == "tr" {
					add(tr)
				}
			}
		case "tr":
			add(k)
		}
	}
	return caption, rows
}

// dataTable reports whether no cell holds a block that a Markdown cell
// can't: a list, a table, a heading, preformatted text or a quote.
func dataTable(rows [][]*Node) bool {
	for _, r := range rows {
		for _, c := range r {
			if c.hasTableBlock {
				return false
			}
		}
	}
	return true
}

// cell renders a table cell's content on one line.
func (w *writer) cell(c *Node) string {
	sub := &writer{ctx: w.ctx, plain: w.plain, base: w.base, limit: maxCell * 4}
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
