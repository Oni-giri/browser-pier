package pagetools

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/Oni-giri/browser-pier/internal/sessions"
)

const (
	// DefaultFindLimit is how many matches find returns unless asked;
	// MaxFindLimit caps what it may ask.
	DefaultFindLimit = 10
	MaxFindLimit     = 50
	// findContext is how much text find shows each side of a match.
	findContext = 80
	// maxQuery caps what find looks for.
	maxQuery = 200
	// maxFindOutput caps find's whole answer.
	maxFindOutput = 20000
)

// FindIn is find's input.
type FindIn struct {
	Text  string `json:"text"`
	Limit int    `json:"limit,omitempty"`
}

// Find looks for text in the active tab, case-insensitively, over what a
// snapshot reads (all of it, not only what fits a budget), and returns
// each match with the text around it and the nearest ref. On a page too
// large to snapshot it reads the DOM snapshot's text alone.
func (t *Tools) Find(ctx context.Context, s sessions.Session, who Caller, in FindIn) (string, error) {
	query := strings.TrimSpace(in.Text)
	if query == "" {
		return "", &Error{Status: 400, Msg: "text: say what to look for"}
	}
	if len([]rune(query)) > maxQuery {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("text: at most %d characters", maxQuery)}
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultFindLimit
	}
	if limit < 1 || limit > MaxFindLimit {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("limit: want 1 to %d", MaxFindLimit)}
	}
	return t.run(ctx, s, who, func(c *call) (string, error) {
		p, err := c.read(c.lease.Ctx, false)
		if err != nil {
			return "", err
		}
		c.st.refs.dropStale(c.tgt.ID, c.loader)
		needle := fold(query)
		var out []string
		total, size := 0, 0
		for _, it := range p.items {
			hay := it.text
			if it.control || it.iframe {
				hay = searchable(it)
			}
			h := fold(hay)
			r := []rune(hay)
			for at := 0; at+len(needle) <= len(h); {
				i := index(h[at:], needle)
				if i < 0 {
					break
				}
				i += at
				total++
				at = i + len(needle)
				if len(out) >= limit || size > maxFindOutput {
					continue
				}
				from, to := max(0, i-findContext), min(len(r), i+len(needle)+findContext)
				ctxText := string(r[from:to])
				if from > 0 {
					ctxText = "…" + ctxText
				}
				if to < len(r) {
					ctxText += "…"
				}
				l := c.ref(p, it.backend) + " " + ctxText
				out = append(out, l)
				size += len(l)
			}
		}
		c.note("Searched the page")
		if total == 0 {
			return c.result("find results", "", "No match."), nil
		}
		trailer := fmt.Sprintf("%d of %d matches.", len(out), total)
		if len(out) == total {
			trailer = fmt.Sprintf("%d matches.", total)
			if total == 1 {
				trailer = "1 match."
			}
		}
		if !p.axRead {
			trailer += " The page is too large to read its controls: refs are the text's elements."
		}
		return c.result("find results", strings.Join(out, "\n"), trailer), nil
	})
}

// searchable is a control's text for find: its role, name and value, as
// a snapshot shows them (secret fields show no value).
func searchable(it item) string {
	if it.iframe {
		return "iframe " + it.name
	}
	s := it.role + " " + quote(strings.Join(strings.Fields(it.name), " "))
	if it.value != "" {
		s += " = " + it.value
	}
	for _, st := range it.states {
		s += " [" + st + "]"
	}
	return s
}

// fold lowercases rune by rune, so its runes line up with s's.
func fold(s string) []rune {
	r := []rune(s)
	for i, c := range r {
		r[i] = unicode.ToLower(c)
	}
	return r
}

func index(h, n []rune) int {
outer:
	for i := 0; i+len(n) <= len(h); i++ {
		for j := range n {
			if h[i+j] != n[j] {
				continue outer
			}
		}
		return i
	}
	return -1
}
