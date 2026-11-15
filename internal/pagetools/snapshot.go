package pagetools

import (
	"context"
	"fmt"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const (
	// DefaultMaxChars is a snapshot's budget unless the agent asks for
	// another; MaxMaxChars caps what it may ask.
	DefaultMaxChars = 4000
	MaxMaxChars     = 20000
	// maxHeader caps the URL (with its query) and the title.
	maxHeader = 256
)

// SnapshotIn is snapshot's input.
type SnapshotIn struct {
	// MaxChars is the budget for the page's controls and text; 0 gives
	// only the URL and title. nil is DefaultMaxChars.
	MaxChars *int `json:"max_chars,omitempty"`
	// Diff answers with what changed since the last snapshot of this
	// document, as act's changes do, instead of the whole page; with no
	// earlier snapshot of it, the whole page (PROJ-114).
	Diff bool `json:"diff,omitempty"`
}

// Snapshot reads the active tab: its URL and title, then, within the
// budget, its controls with refs and its text, the open modal first, then
// what is in view, the main content and the rest. A marker says what was
// left out.
func (t *Tools) Snapshot(ctx context.Context, s sessions.Session, who Caller, in SnapshotIn) (string, error) {
	budget := DefaultMaxChars
	if in.MaxChars != nil {
		budget = *in.MaxChars
	}
	if budget < 0 || budget > MaxMaxChars {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("max_chars: want 0 to %d", MaxMaxChars)}
	}
	if in.Diff && budget == 0 {
		return "", &Error{Status: 400, Msg: "diff: want a max_chars above 0, for when there is no earlier snapshot to compare with"}
	}
	return t.run(ctx, s, who, func(c *call) (string, error) {
		ctx := c.lease.Ctx
		var p *page
		if in.Diff {
			p, r, err := c.changes(ctx, true)
			if err != nil {
				return "", err
			}
			c.note("Read the page")
			c.noteChallenge(p.cf)
			if r.kind == diffDone {
				return c.result("page changes", r.body, r.trailer), nil
			}
			// Nothing to compare with: the whole page, which is now the
			// one diffs compare with (unless it changed while it was read).
			lead := "No earlier snapshot of this page to compare with; the whole page follows."
			if r.kind != diffFirst {
				lead = "The page navigated since the last snapshot; the whole page follows."
			}
			body, marker := c.render(p, budget)
			return lead + "\n" + c.result("page snapshot", body, marker), nil
		}
		if budget == 0 {
			pi, err := c.info(ctx)
			if err != nil {
				return "", err
			}
			p = &page{url: pi.URL, title: pi.Title, cf: pi.CF}
		} else {
			var err error
			var moved bool
			if p, moved, err = c.readDoc(ctx); err != nil {
				return "", err
			}
			c.st.refs.dropStale(c.tgt.ID, p.loader)
			if moved {
				c.t.dropBaseline(c.sess.ID, c.tgt.ID)
			} else {
				// What diffs compare against (PROJ-114).
				c.t.setBaseline(c.sess.ID, c.tgt.ID, p.frame, p.loader, p.items)
			}
		}
		body, marker := c.render(p, budget)
		c.note("Read the page")
		c.noteChallenge(p.cf)
		return c.result("page snapshot", body, marker), nil
	})
}

// header is the URL and title lines.
func header(p *page) string {
	return "URL: " + capText(p.url, maxHeader) + "\nTitle: " + capText(strings.Join(strings.Fields(p.title), " "), maxHeader)
}

// line is how an item reads in a snapshot; controls and iframes get a ref.
func (c *call) line(p *page, it item) string {
	if !it.control && !it.iframe {
		return it.text
	}
	ref := c.ref(p, it.backend)
	if it.iframe {
		return ref + " iframe " + quoteOdd(it.name) + " (not read)"
	}
	var b strings.Builder
	b.WriteString(ref + " " + it.role + " " + quote(capText(strings.Join(strings.Fields(it.name), " "), maxField)))
	if it.value != "" {
		b.WriteString(" = " + it.value)
	}
	for _, st := range it.states {
		b.WriteString(" [" + st + "]")
	}
	return b.String()
}

func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `\"`) + `"` }

// ref is the ref of a node of the page's document.
func (c *call) ref(p *page, backend int64) string {
	return c.st.refs.ref(refKey{target: c.tgt.ID, frame: p.frame, loader: p.loader, backend: backend})
}

// render spends the budget in tier order (controls before text within
// each tier) and says what didn't fit.
func (c *call) render(p *page, budget int) (body, marker string) {
	var b strings.Builder
	b.WriteString(header(p))
	if budget == 0 {
		return b.String(), ""
	}
	left := budget
	omittedControls, omittedChars := 0, 0
	full := false
	summed := map[int]bool{}
	for tier := range nTiers {
		var lines []string
		for _, pass := range []bool{true, false} { // controls, then text
			for _, it := range p.items {
				if it.tier != tier || (it.control || it.iframe) != pass {
					continue
				}
				if g := p.groups[it.group]; g != nil && g.controls > navLimit {
					// A big navigation or menu: one line, in place of its first control.
					if summed[it.group] || full {
						continue
					}
					summed[it.group] = true
					if l := c.summary(p, it.group, g); len([]rune(l))+1 <= left {
						lines = append(lines, l)
						left -= len([]rune(l)) + 1
					} else {
						full = true
						omittedControls += g.controls
					}
					continue
				}
				if full {
					if pass {
						omittedControls++
					} else {
						omittedChars += len([]rune(it.text))
					}
					continue
				}
				l := c.lineIfFits(p, it, left)
				if l == "" {
					full = true
					if pass {
						omittedControls++
					} else {
						// Text is cut to fit rather than dropped whole.
						n := len([]rune(it.text))
						if left > 40 {
							l = capText(it.text, left-1)
							omittedChars += n - len([]rune(l)) + 1
							lines = append(lines, l)
							left = 0
						} else {
							omittedChars += n
						}
					}
					continue
				}
				lines = append(lines, l)
				left -= len([]rune(l)) + 1
			}
		}
		if len(lines) > 0 {
			b.WriteString("\n-- " + tierNames[tier] + " --\n")
			b.WriteString(strings.Join(lines, "\n"))
		}
	}
	if !p.axRead {
		b.WriteString("\n(the page is too large to read its controls; text only)")
	}
	if omittedControls > 0 || omittedChars > 0 {
		var parts []string
		if omittedControls > 0 {
			parts = append(parts, thousands(omittedControls)+" controls")
		}
		if omittedChars > 0 {
			parts = append(parts, thousands(omittedChars)+" characters")
		}
		marker = "…(" + strings.Join(parts, " and ") + " omitted; use find)"
	}
	return b.String(), marker
}

// summary is the line that stands for a big navigation or menu: how many
// controls it has and the first few names.
func (c *call) summary(p *page, id int, g *group) string {
	var names []string
	for _, it := range p.items {
		if it.group == id {
			names = append(names, quote(capText(strings.Join(strings.Fields(it.name), " "), 30)))
			if len(names) == 5 {
				break
			}
		}
	}
	label := g.role
	if g.name != "" {
		label += " " + quote(capText(collapse(g.name), maxField))
	}
	return fmt.Sprintf("%s with %d controls: %s… (use find for them)", label, g.controls, strings.Join(names, ", "))
}

// lineIfFits is the item's line if it fits in left characters, else "".
// A control's ref is only made once its line is shown.
func (c *call) lineIfFits(p *page, it item, left int) string {
	if !it.control && !it.iframe {
		if len([]rune(it.text))+1 > left {
			return ""
		}
		return it.text
	}
	// Estimate before making a ref, so refs aren't spent on lines that
	// won't be shown.
	est := len(it.role) + len([]rune(capText(it.name, maxField))) + len([]rune(it.value)) + 16
	for _, st := range it.states {
		est += len(st) + 3
	}
	if est > left {
		return ""
	}
	l := c.line(p, it)
	if len([]rune(l))+1 > left {
		return ""
	}
	return l
}
