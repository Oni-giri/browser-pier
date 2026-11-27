// Package visualtree is Visual mode's element tree (PROJ-150): it reads
// the page of a Visual-mode browser as text with refs, from the
// accessibility bus in the browser container (the entrypoint's tree
// reader, images/chromium/entrypoint/tree.go), and turns a ref back into
// a point on the screen for a click. No CDP is involved: in Visual mode
// nothing may attach to the page (SPEC §10), and this package imports
// nothing that speaks CDP.
//
// Everything the reader sends is page-controlled: names, text, the URL
// and title. Roles and states come as numbers and are printed as
// BrowserPier's own words; strings are capped, collapsed onto one line
// and fenced (internal/fence); secret fields print [filled] or [empty]
// only. The reader's JSON is never logged.
package visualtree

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
)

// Tree is the reader's GET /tree answer.
type Tree struct {
	Doc          Doc    `json:"doc"`
	ScreenWidth  int    `json:"screen_width"`
	ScreenHeight int    `json:"screen_height"`
	Nodes        []Node `json:"nodes"`
	// Cut is why the read stopped short (nodes, depth or time), or "".
	Cut string `json:"cut,omitempty"`
}

// Doc is the document read: the active tab's page.
type Doc struct {
	App   string `json:"app"`
	ID    string `json:"id"`
	URL   string `json:"url"`
	Title string `json:"title"`
	X     int    `json:"x"`
	Y     int    `json:"y"`
	W     int    `json:"w"`
	H     int    `json:"h"`
}

// Node is one element, in document order.
type Node struct {
	ID     string            `json:"id"`
	Parent int               `json:"parent"`
	Depth  int               `json:"depth"`
	Role   uint32            `json:"role"`
	States uint64            `json:"states"`
	Name   string            `json:"name"`
	Text   string            `json:"text"`
	Filled *bool             `json:"filled"`
	Value  *float64          `json:"value"`
	X      int               `json:"x"`
	Y      int               `json:"y"`
	W      int               `json:"w"`
	H      int               `json:"h"`
	Attrs  map[string]string `json:"attrs"`
	// AttrsRead: the reader read the element's attributes, so Attrs is
	// what it has. A field whose attributes weren't read can't be judged
	// secret or not by its id or name, and prints no value.
	AttrsRead bool `json:"attrs_read"`
	Kids      int  `json:"kids"`
}

// NodeNow is the reader's GET /tree/node answer: one element read again.
type NodeNow struct {
	Stale  bool    `json:"stale"`
	Role   uint32  `json:"role"`
	States uint64  `json:"states"`
	Name   string  `json:"name"`
	X      int     `json:"x"`
	Y      int     `json:"y"`
	W      int     `json:"w"`
	H      int     `json:"h"`
	Doc    Doc     `json:"doc"`
	At     *AtNode `json:"at"`
	// The screen's size, which the taskbar's bottom strip is part of.
	ScreenWidth  int `json:"screen_width"`
	ScreenHeight int `json:"screen_height"`
}

// AtNode is what the hit test found at an element's centre instead of it.
type AtNode struct {
	ID       string `json:"id"`
	Role     uint32 `json:"role"`
	Name     string `json:"name"`
	Contains bool   `json:"contains"`
}

// maxTree caps the reader's answer: about 20,000 nodes with long names.
const maxTree = 32 << 20

// ReadError is a read that failed, with the status the API answers.
type ReadError struct {
	Status int
	// Msg is BrowserPier's words, then the reader's fenced.
	Msg string
}

func (e *ReadError) Error() string { return e.Msg }

// reader reaches a container's tree reader through its control socket.
type reader struct {
	sock string
}

func (r reader) get(ctx context.Context, target string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://control"+target, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", r.sock)
		},
		DisableKeepAlives: true,
	}}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTree+1))
	if err != nil {
		return err
	}
	if len(body) > maxTree {
		return &ReadError{Status: http.StatusBadGateway, Msg: "the browser's element tree is too large to read"}
	}
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		if strings.HasPrefix(string(body), "404 page not found") {
			// No /tree at all: an image from before PROJ-150, or an
			// API-mode container.
			return &ReadError{Status: http.StatusConflict, Msg: "this browser doesn't publish its element tree (restart it in Visual mode, on a newer browser image)"}
		}
		return &ReadError{Status: http.StatusConflict, Msg: "the page's elements aren't there to read: no page is showing in the browser window, or Chromium is still building a big page's tree. Try again in a moment, or take a screenshot"}
	case http.StatusGatewayTimeout:
		return &ReadError{Status: http.StatusGatewayTimeout, Msg: "reading the page's elements took too long; the page may be very large or busy. Take a screenshot instead"}
	default:
		return &ReadError{Status: http.StatusBadGateway, Msg: "the browser couldn't read the page's elements: " + fenceError(string(body))}
	}
	if err := json.Unmarshal(body, out); err != nil {
		// The decoder quotes types, never the values it refused.
		return &ReadError{Status: http.StatusBadGateway, Msg: "the browser's element tree couldn't be read: " + fenceError(err.Error())}
	}
	return nil
}

// read reads the active tab's page.
func (r reader) read(ctx context.Context) (*Tree, error) {
	var t Tree
	if err := r.get(ctx, "/tree", &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// node reads one element again.
func (r reader) node(ctx context.Context, k refKey) (*NodeNow, error) {
	q := url.Values{"app": {k.app}, "doc": {k.doc}, "id": {k.id}}
	var n NodeNow
	if err := r.get(ctx, "/tree/node?"+q.Encode(), &n); err != nil {
		return nil, err
	}
	return &n, nil
}

func fenceError(s string) string {
	return wrap("browser error", capText(strings.TrimSpace(s), 500))
}
