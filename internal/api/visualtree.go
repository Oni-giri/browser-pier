package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/visualtree"
)

// Visual mode's element tree (PROJ-150, internal/visualtree): the page
// read from the accessibility bus, with no CDP, and clicks by its refs.

func (a *API) registerVisualTree(mux Router) {
	mux.Handle("POST /api/sessions/{id}/visual-snapshot", a.agent(a.visualSnapshot))
}

// VisualTree is the element tree tools the API runs, for tests.
func (a *API) VisualTree() *visualtree.Tools { return a.tree }

func (a *API) visualSnapshot(w http.ResponseWriter, r *http.Request) {
	s, ok := a.session(w, r)
	if !ok {
		return
	}
	var in visualtree.SnapshotIn
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	if err != nil {
		writeError(w, http.StatusBadRequest, "body too large")
		return
	}
	if strings.TrimSpace(string(body)) != "" {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&in); err != nil {
			writeError(w, http.StatusBadRequest, "bad JSON: "+err.Error())
			return
		}
	}
	tok, _ := auth.TokenFromContext(r.Context())
	out, err := a.tree.Snapshot(r.Context(), s, visualtree.Caller{TokenID: tok.ID, Agent: tok.Name}, in)
	if !a.treeError(w, r, s, err) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": out})
}

// treeError writes a tree call's error, reporting whether there was none.
func (a *API) treeError(w http.ResponseWriter, r *http.Request, s sessions.Session, err error) bool {
	var te *visualtree.Error
	switch {
	case err == nil:
		return true
	case errors.As(err, &te):
		writeError(w, te.Status, te.Msg)
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		// the client went away
	default:
		a.pageInternal(w, s, "reading the element tree", err)
	}
	return false
}

// refClick is a click by a Visual-mode ref: where to click, what it is,
// and the ref, which the activity log names instead of the point.
type refClick struct {
	ref    string
	target visualtree.Target
}

// locateRef turns a click's ref into the element's centre as it is now,
// or writes why it can't be clicked.
func (a *API) locateRef(w http.ResponseWriter, r *http.Request, s sessions.Session, ref string) (*refClick, bool) {
	tok, _ := auth.TokenFromContext(r.Context())
	t, err := a.tree.Locate(r.Context(), s, visualtree.Caller{TokenID: tok.ID, Agent: tok.Name}, ref)
	if !a.treeError(w, r, s, err) {
		return nil, false
	}
	if a.afterLocate != nil {
		a.afterLocate()
	}
	return &refClick{ref: ref, target: t}, true
}

// generation is the lease generation a ref click's point was read
// under, for AgentInputAt, or nil for a click that isn't by ref.
func (rc *refClick) generation() *uint64 {
	if rc == nil {
		return nil
	}
	return &rc.target.Generation
}

// clickedText is a ref click's answer: what it clicked, fenced.
func (rc *refClick) clickedText() string { return "Clicked " + rc.ref + ", " + rc.target.What }

// takeRef takes ref out of a click's JSON body and puts the element's
// point in its place, or writes why it can't. A body without a ref comes
// back unchanged, with a nil click.
func (a *API) takeRef(w http.ResponseWriter, r *http.Request, s sessions.Session, body []byte) ([]byte, *refClick, bool) {
	var fields map[string]json.RawMessage
	if json.Unmarshal(body, &fields) != nil {
		return body, nil, true // not an object: the browser refuses it
	}
	raw, ok := fields["ref"]
	if !ok {
		return body, nil, true
	}
	var ref string
	if err := json.Unmarshal(raw, &ref); err != nil || ref == "" {
		writeError(w, http.StatusBadRequest, "ref: want a ref from visual_snapshot, like vk3n12")
		return nil, nil, false
	}
	_, hasX := fields["x"]
	_, hasY := fields["y"]
	if hasX || hasY {
		writeError(w, http.StatusBadRequest, "click: give a ref or x and y, not both")
		return nil, nil, false
	}
	rc, ok := a.locateRef(w, r, s, ref)
	if !ok {
		return nil, nil, false
	}
	delete(fields, "ref")
	fields["x"], _ = json.Marshal(rc.target.X)
	fields["y"], _ = json.Marshal(rc.target.Y)
	out, err := json.Marshal(fields)
	if err != nil {
		a.internal(w, "encoding an action", err)
		return nil, nil, false
	}
	return out, rc, true
}
