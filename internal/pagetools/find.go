package pagetools

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

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
		nlen := utf8.RuneCountInString(needle)
		var out []string
		total, size := 0, 0
		for k, it := range p.items {
			if k%checkEvery == 0 {
				if err := c.lease.Ctx.Err(); err != nil {
					return "", err
				}
			}
			hay := it.text
			if it.control || it.iframe {
				hay = searchable(it)
			}
			// fold maps rune to rune, so h's characters line up with
			// hay's; byte offsets in h become character offsets as found.
			h := fold(hay)
			var r []rune // hay's characters, made for the first match shown
			runeAt, byteAt := 0, 0
			for at := 0; ; {
				i := strings.Index(h[at:], needle)
				if i < 0 {
					break
				}
				i += at
				total++
				at = i + len(needle)
				if len(out) >= limit || size > maxFindOutput {
					continue
				}
				runeAt += utf8.RuneCountInString(h[byteAt:i])
				byteAt = i
				if r == nil {
					r = []rune(hay)
				}
				from, to := max(0, runeAt-findContext), min(len(r), runeAt+nlen+findContext)
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
		trailer := fmt.Sprintf("%d of %d matches.", len(out), total)
		switch {
		case total == 0:
			trailer = "No match."
		case len(out) == total && total == 1:
			trailer = "1 match."
		case len(out) == total:
			trailer = fmt.Sprintf("%d matches.", total)
		}
		if !p.axRead {
			trailer += " The page is too large to read its controls: refs are the text's elements."
		}
		if p.textCut {
			trailer += " The page has more text than BrowserPier reads (4 MB): some of it was left out of the search."
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

// fold lowercases rune by rune, so its runes line up with s's (an
// invalid byte stays one character, as []rune makes it).
func fold(s string) string { return strings.Map(unicode.ToLower, s) }
