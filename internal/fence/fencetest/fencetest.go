// Package fencetest reads the page envelopes (PROJ-167) in a tool's
// answer, for tests.
package fencetest

import (
	"fmt"
	"regexp"
	"strings"
)

// Envelope is one page envelope as an answer has it.
type Envelope struct {
	// Marker is the fence's, which the content tag carries too; Source is
	// the fence's source label.
	Marker, Source string
	URL, Title     string
	HasTitle       bool
	Format         string
	// Content is what is inside the content tag, without its last line
	// break; After is what follows </page> inside the fence (notices).
	Content, After string
}

var fenceOpen = regexp.MustCompile(`<untrusted-content-([0-9a-f]{16}) source="([^"]*)">\n[^\n]*\n<page>\n`)

// Pages are out's page envelopes, in order, or an error naming the first
// that isn't whole.
func Pages(out string) ([]Envelope, error) {
	var all []Envelope
	for _, m := range fenceOpen.FindAllStringSubmatchIndex(out, -1) {
		e := Envelope{Marker: out[m[2]:m[3]], Source: out[m[4]:m[5]]}
		rest := out[m[1]:]
		end := strings.Index(rest, "\n</untrusted-content-"+e.Marker+">")
		if end < 0 {
			return nil, fmt.Errorf("an envelope's fence isn't closed")
		}
		rest = rest[:end]
		u, ok := strings.CutPrefix(rest, "<url>")
		if !ok {
			return nil, fmt.Errorf("no <url> first")
		}
		i := strings.Index(u, "</url>\n")
		if i < 0 {
			return nil, fmt.Errorf("<url> isn't closed")
		}
		e.URL, rest = u[:i], u[i+len("</url>\n"):]
		if tl, ok := strings.CutPrefix(rest, "<title>"); ok {
			i := strings.Index(tl, "</title>\n")
			if i < 0 {
				return nil, fmt.Errorf("<title> isn't closed")
			}
			e.Title, e.HasTitle, rest = tl[:i], true, tl[i+len("</title>\n"):]
		}
		c, ok := strings.CutPrefix(rest, "<page_content-"+e.Marker+` format="`)
		if !ok {
			return nil, fmt.Errorf("no content tag with the fence's marker")
		}
		i = strings.Index(c, "\">\n")
		if i < 0 {
			return nil, fmt.Errorf("the content tag isn't whole")
		}
		e.Format, c = c[:i], c[i+3:]
		closing := "</page_content-" + e.Marker + ">"
		j := strings.Index(c, closing+"\n</page>")
		if j < 0 || strings.Count(c, closing) != 1 {
			return nil, fmt.Errorf("the content tag isn't closed once, then </page>")
		}
		e.Content = strings.TrimSuffix(c[:j], "\n")
		e.After = strings.TrimPrefix(c[j+len(closing+"\n</page>"):], "\n")
		all = append(all, e)
	}
	return all, nil
}

// Page is out's one page envelope, or an error if it hasn't exactly one.
func Page(out string) (Envelope, error) {
	all, err := Pages(out)
	if err != nil {
		return Envelope{}, err
	}
	if len(all) != 1 {
		return Envelope{}, fmt.Errorf("%d page envelopes, want 1", len(all))
	}
	return all[0], nil
}
