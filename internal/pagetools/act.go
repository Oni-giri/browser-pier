package pagetools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/cdp"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/motion"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// MaxSteps caps one act call.
const MaxSteps = 20

// maxText caps what one fill or type step puts in.
const maxText = 10000

// Step is one action of an act call.
type Step struct {
	// Action is click, fill, type, press, select, check, uncheck, hover
	// or upload.
	Action string `json:"action"`
	Ref    string `json:"ref,omitempty"`
	// Button is left (the default), right or middle; Double double-clicks.
	Button string `json:"button,omitempty"`
	Double bool   `json:"double,omitempty"`
	// Text is what fill puts in the field, replacing what was there, or
	// what type types after it.
	Text *string `json:"text,omitempty"`
	// Keys is what press presses: "Enter", "ctrl+a", or several
	// separated by spaces.
	Keys string `json:"keys,omitempty"`
	// Option is what select chooses, by value or visible text.
	Option string `json:"option,omitempty"`
	// Files is what upload sets on a file input: the names of files in
	// the session's uploads (upload_file, POST /api/sessions/{id}/files),
	// bare names only (PROJ-117).
	Files []string `json:"files,omitempty"`
	// Pace is human (the default: the pointer glides to a point scattered
	// around the element's middle, buttons and keys are held, and type
	// keys at a typist's rhythm) or instant (PROJ-113). It applies to
	// click, hover, check, uncheck, type and press.
	Pace string `json:"pace,omitempty"`
}

// instant reports whether a step runs at instant pace.
func (s Step) instant() bool { return s.Pace == paceInstant }

const (
	paceHuman   = "human"
	paceInstant = "instant"
	// maxHumanType caps what one act call types at human pace, about a
	// quarter of a second a character: 500 characters is two minutes.
	maxHumanType = 500
	// maxUploadFiles caps one upload step.
	maxUploadFiles = 10
)

// ActIn is act's input.
type ActIn struct {
	Steps []Step `json:"steps"`
	// Snapshot appends a snapshot of the page after the steps.
	Snapshot bool `json:"snapshot,omitempty"`
}

// checkStep says what's wrong with a step, before any runs.
func checkStep(s Step) error {
	switch s.Pace {
	case "", paceHuman, paceInstant:
	default:
		return fmt.Errorf("pace %q: want human or instant", s.Pace)
	}
	needRef := func() error {
		if s.Ref == "" {
			return fmt.Errorf("%s: want a ref from snapshot or find", s.Action)
		}
		return nil
	}
	switch s.Action {
	case "click":
		switch s.Button {
		case "", "left", "right", "middle":
		default:
			return errors.New("click: button is left, right or middle")
		}
		return needRef()
	case "hover", "check", "uncheck":
		return needRef()
	case "fill", "type":
		if s.Text == nil {
			return fmt.Errorf("%s: want text", s.Action)
		}
		if utf8.RuneCountInString(*s.Text) > maxText {
			return fmt.Errorf("%s: at most %d characters", s.Action, maxText)
		}
		if s.Action == "type" && *s.Text == "" {
			return errors.New("type: want text")
		}
		return needRef()
	case "select":
		if s.Option == "" {
			return errors.New("select: want the option, by value or visible text")
		}
		return needRef()
	case "press":
		_, err := parseKeys(s.Keys)
		return err
	case "upload":
		if len(s.Files) == 0 || len(s.Files) > maxUploadFiles {
			return fmt.Errorf("upload: want 1 to %d file names, from upload_file", maxUploadFiles)
		}
		for i, name := range s.Files {
			if err := checkUploadName(name); err != nil {
				return fmt.Errorf("upload: file %d: %w", i+1, err)
			}
		}
		return needRef()
	case "":
		return errors.New("each step needs an action: " + actions)
	}
	return fmt.Errorf("action %q: want %s", s.Action, actions)
}

const actions = "click, fill, type, press, select, check, uncheck, hover or upload"

// checkUploadName says what's wrong with a file name an upload step
// names: only a bare name of a file in the uploads folder will do, never
// a path (files.ValidName, and no "..", to keep well clear of one).
func checkUploadName(name string) error {
	switch {
	case name == "":
		return errors.New("an empty name")
	case len(name) > 255:
		return errors.New("a name is at most 255 bytes")
	case strings.ContainsAny(name, `/\`) || strings.Contains(name, ".."):
		return errors.New("give the file's bare name, as upload_file returned it, without folders or \"..\"")
	case strings.ContainsRune(name, 0):
		return errors.New("a name can't hold a NUL")
	case !files.ValidName(name):
		return errors.New("not a valid file name: it is hidden (starts with a dot) or has an unprintable character")
	}
	return nil
}

// Act runs up to 20 steps on the active tab, in order, stopping at the
// first that fails. Each step's result echoes the element's role and name
// as they are when it acts, so the agent sees what it actually hit; typed
// text is never echoed, only counted.
//
// Pointer steps scroll the element into view, find its centre, check in
// the isolated world that it isn't covered (walking up through shadow
// roots, closed ones too; the element's own label counts as it), then
// dispatch the mouse events at once. Typing steps focus the element and
// check it has the focus before and after. The page can still change in
// the milliseconds between a check and the input: act resolves, checks
// and dispatches back to back and echoes what it hit, but promises no more.
func (t *Tools) Act(ctx context.Context, s sessions.Session, who Caller, in ActIn) (string, error) {
	if len(in.Steps) == 0 || len(in.Steps) > MaxSteps {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("steps: want 1 to %d", MaxSteps)}
	}
	human := 0
	for i, st := range in.Steps {
		if err := checkStep(st); err != nil {
			return "", &Error{Status: 400, Msg: fmt.Sprintf("step %d: %v", i+1, err)}
		}
		if st.Action == "type" && !st.instant() {
			human += utf8.RuneCountInString(*st.Text)
		}
	}
	if human > maxHumanType {
		return "", &Error{Status: 400, Msg: fmt.Sprintf("type steps: %d characters at human pace, at most %d per call (about two minutes); split them across calls, use fill, or set pace instant", human, maxHumanType)}
	}
	return t.run(ctx, s, who, func(c *call) (string, error) {
		ctx := c.lease.Ctx
		var done []string
		for i, st := range in.Steps {
			if i > 0 && c.t.settle > 0 {
				// Let the page react to the last step before the next.
				select {
				case <-ctx.Done():
				case <-time.After(c.t.settle):
				}
			}
			c.instant = st.instant()
			line, err := c.step(ctx, st)
			if err != nil {
				return "", c.stepFailed(done, i, len(in.Steps), st, err)
			}
			done = append(done, line)
		}
		// A step may have opened a tab.
		_, _ = c.listTabs(ctx)
		out := c.result("page actions", strings.Join(done, "\n"), "")
		if in.Snapshot {
			p, err := c.read(ctx, true)
			if err != nil {
				return out + "\nThe snapshot after it failed: " + c.fail(err).Error(), nil
			}
			c.notices = nil
			body, marker := c.render(p, DefaultMaxChars)
			out += "\n" + c.result("page snapshot", body, marker)
		}
		return out, nil
	})
}

// stepFailed is act's answer when step i failed: what the earlier steps
// did, fenced, then what went wrong and what didn't run.
func (c *call) stepFailed(done []string, i, n int, st Step, err error) error {
	err = c.fail(err)
	status := http.StatusBadGateway
	var e *Error
	if errors.As(err, &e) {
		status = e.Status
	}
	msg := ""
	if len(done) > 0 || len(c.notices) > 0 {
		msg = c.result("page actions", strings.Join(done, "\n"), "") + "\n"
	}
	msg += fmt.Sprintf("Step %d (%s) failed: %s", i+1, st.Action, err.Error())
	if rest := n - i - 1; rest == 1 {
		msg += " The step after it didn't run."
	} else if rest > 1 {
		msg += fmt.Sprintf(" The %d steps after it didn't run.", rest)
	}
	return &Error{Status: status, Msg: msg}
}

// node is a ref resolved in the active tab: its backend node, its object
// in the isolated world, and its role and name as AX gives them now.
type node struct {
	ref        string
	backend    int64
	obj        string
	role, name string
}

// echo is how a step's result names what it acted on.
func (n *node) echo() string {
	return n.ref + " " + n.role + " " + quote(capText(strings.Join(strings.Fields(n.name), " "), maxField))
}

// resolve finds what a ref names, now. A ref from another tab is refused;
// one whose document has changed, or whose node is gone, is stale.
func (c *call) resolve(ctx context.Context, ref string) (*node, error) {
	k, err := c.st.refs.lookup(ref)
	if err != nil {
		return nil, err
	}
	if k.target != c.tgt.ID {
		where := "a tab that isn't open any more"
		if tb := c.st.tabs[k.target]; tb != nil {
			where = "another tab (" + tb.id + ")"
		}
		return nil, &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("%s is in %s, not the active one; switch to it with tabs, or take a new snapshot", ref, where)}
	}
	if err := c.refresh(ctx); err != nil {
		return nil, err
	}
	if k.loader != c.loader || k.frame != c.frame {
		return nil, errStale(ref)
	}
	cctx, cancel := context.WithTimeout(ctx, c.t.quick)
	obj, err := c.tgt.ResolveNode(cctx, c.world, k.backend)
	cancel()
	if err != nil {
		var ce *cdp.Error
		if errors.As(err, &ce) || errors.Is(err, cdp.ErrWorldGone) {
			return nil, errStale(ref)
		}
		return nil, c.timedOut(ctx, err)
	}
	n := &node{ref: ref, backend: k.backend, obj: obj}
	var connected bool
	if err := c.callOn(ctx, n.obj, jsConnected, nil, &connected); err != nil {
		return nil, err
	}
	if !connected {
		return nil, errStale(ref)
	}
	n.role, n.name = c.axName(ctx, k.backend)
	return n, nil
}

// axName is a node's role and name as the AX tree gives them, from one
// node (Accessibility.getPartialAXTree), not the whole tree.
func (c *call) axName(ctx context.Context, backend int64) (role, name string) {
	var r struct {
		Nodes []axNode `json:"nodes"`
	}
	err := c.renderer(ctx, c.t.quick, "Accessibility.getPartialAXTree", map[string]any{"backendNodeId": backend, "fetchRelatives": false}, &r)
	if err != nil || len(r.Nodes) == 0 {
		return "element", ""
	}
	n := r.Nodes[0]
	role = n.Role.String()
	if n.Ignored || role == "" || role == "none" || role == "generic" {
		role = "element"
	}
	return role, n.Name.String()
}

// describe names an element the page gave back as an object (what covers
// a ref, what took the focus): its role and name, fenced.
func (c *call) describe(ctx context.Context, obj string) string {
	var d struct {
		Node struct {
			BackendNodeID int64  `json:"backendNodeId"`
			NodeName      string `json:"nodeName"`
		} `json:"node"`
	}
	what := "something"
	if c.renderer(ctx, c.t.quick, "DOM.describeNode", map[string]any{"objectId": obj}, &d) == nil {
		role, name := c.axName(ctx, d.Node.BackendNodeID)
		if role == "element" {
			role = strings.ToLower(d.Node.NodeName)
		}
		what = role + " " + quote(capText(strings.Join(strings.Fields(name), " "), maxField))
	}
	return fence.Wrap("page element", what)
}

// callOn calls a function on an isolated-world object, returning its
// value as JSON into out.
func (c *call) callOn(ctx context.Context, obj, fn string, args []any, out any) error {
	r, err := c.callOnObject(ctx, obj, fn, args, true)
	if err != nil {
		return err
	}
	if out != nil && len(r.Value) > 0 {
		return json.Unmarshal(r.Value, out)
	}
	return nil
}

func (c *call) callOnObject(ctx context.Context, obj, fn string, args []any, byValue bool) (cdp.RemoteObject, error) {
	as := make([]map[string]any, len(args))
	for i, a := range args {
		as[i] = map[string]any{"value": a}
	}
	var r struct {
		Result           cdp.RemoteObject      `json:"result"`
		ExceptionDetails *cdp.ExceptionDetails `json:"exceptionDetails"`
	}
	err := c.renderer(ctx, c.t.quick, "Runtime.callFunctionOn", map[string]any{"objectId": obj, "functionDeclaration": fn,
		"arguments": as, "returnByValue": byValue, "silent": true}, &r)
	if err != nil {
		return r.Result, err
	}
	if r.ExceptionDetails != nil {
		return r.Result, fmt.Errorf("script error: %s", r.ExceptionDetails.Text)
	}
	return r.Result, nil
}

// The scripts act runs in the isolated world. Each starts with a marker
// comment, which the tests' fake browser goes by.
const (
	jsConnected = `/*bp:connected*/ function() { return this.isConnected; }`
	// jsHitTest checks nothing covers the element at (x, y): at each root
	// from the element's own up to the document, the topmost element there
	// must be the element (or, a level up, its shadow host), something
	// inside it, or one of its labels. It returns true, false if nothing
	// is there, or what covers it.
	jsHitTest = `/*bp:hittest*/ function(x, y) {
  const labels = this.labels ? Array.from(this.labels) : [];
  let el = this;
  let root = el.getRootNode();
  for (;;) {
    const hit = root.elementFromPoint(x, y);
    if (!hit) return false;
    const ok = hit === el || el.contains(hit) || (root.host && hit === root.host) ||
      labels.some(l => l === hit || l.contains(hit));
    if (!ok) return hit;
    if (!(root instanceof ShadowRoot)) return true;
    el = root.host;
    root = el.getRootNode();
  }
}`
	jsFocused = `/*bp:focused*/ function() { return this.getRootNode().activeElement === this; }`
	jsActive  = `/*bp:active*/ function() { let a = this.getRootNode().activeElement; return a || null; }`
	// jsField says what kind of field an element is.
	jsField = `/*bp:field*/ function() {
  return {tag: this.localName, type: (this.getAttribute('type') || '').toLowerCase(), editable: !!this.isContentEditable,
    disabled: !!this.disabled, checked: typeof this.checked === 'boolean' ? this.checked : null,
    aria: this.getAttribute('aria-checked'), role: this.getAttribute('role'),
    file: (this.localName === 'input' && this.type === 'file') || !!(this.control && this.control.type === 'file')};
}`
	// jsFileInput is the file input an upload step sets: the element, if
	// it is one, or the control of a label, if that is one; else null.
	jsFileInput = `/*bp:fileinput*/ function() {
  const file = e => !!e && e.localName === 'input' && e.type === 'file';
  if (file(this)) return this;
  if (this.localName === 'label' && file(this.control)) return this.control;
  return null;
}`
	// jsFileInfo says what a file input takes and holds.
	jsFileInfo = `/*bp:fileinfo*/ function() {
  return {multiple: !!this.multiple, disabled: !!this.disabled, count: this.files ? this.files.length : 0};
}`
	// jsSelect chooses a <select>'s option by value, else by its text, as
	// Playwright does: it sets the option and dispatches input and change.
	jsSelect = `/*bp:select*/ function(want) {
  if (this.localName !== 'select') return {error: 'not a select'};
  if (this.disabled) return {error: 'disabled'};
  const opts = Array.from(this.options);
  const norm = s => s.replace(/\s+/g, ' ').trim();
  const o = opts.find(o => o.value === want) || opts.find(o => norm(o.text) === norm(want)) ||
    opts.find(o => norm(o.text).toLowerCase() === norm(want).toLowerCase());
  if (!o) return {error: 'no option', options: opts.slice(0, 10).map(o => norm(o.text))};
  if (o.disabled) return {error: 'option disabled'};
  o.selected = true;
  this.dispatchEvent(new Event('input', {bubbles: true, composed: true}));
  this.dispatchEvent(new Event('change', {bubbles: true}));
  return {text: norm(o.text)};
}`
)

// step runs one step and returns its result line.
func (c *call) step(ctx context.Context, st Step) (string, error) {
	if st.Action == "press" {
		combos, _ := parseKeys(st.Keys)
		for _, cb := range combos {
			if err := c.pressCombo(ctx, cb); err != nil {
				return "", err
			}
		}
		c.note(describeKeys(combos))
		return "pressed " + st.Keys, nil
	}
	n, err := c.resolve(ctx, st.Ref)
	if err != nil {
		return "", err
	}
	switch st.Action {
	case "click":
		// A file chooser is a window of the browser's own: nothing over
		// CDP could close it, and it would block the person too.
		var f fieldInfo
		if err := c.callOn(ctx, n.obj, jsField, nil, &f); err != nil {
			return "", err
		}
		if f.File {
			return "", &Error{Status: 422, Msg: n.ref + " opens a file chooser, which act doesn't click; put the file in the session with upload_file (or POST /api/sessions/{id}/files), then use an upload step on " + n.ref}
		}
		x, y, err := c.aim(ctx, n)
		if err != nil {
			return "", err
		}
		button := st.Button
		if button == "" {
			button = "left"
		}
		if err := c.click(ctx, n, x, y, button, st.Double); err != nil {
			return "", err
		}
		what := map[string]string{"left": "Clicked", "right": "Right-clicked", "middle": "Middle-clicked"}[button]
		if st.Double {
			what = "Double-clicked"
		}
		c.note(what + " " + n.ref)
		return strings.ToLower(what) + " " + n.echo(), nil
	case "hover":
		x, y, err := c.aim(ctx, n)
		if err != nil {
			return "", err
		}
		if _, _, err := c.glide(ctx, n, x, y); err != nil {
			return "", err
		}
		c.note("Hovered over " + n.ref)
		return "hovered over " + n.echo(), nil
	case "fill", "type":
		return c.typeInto(ctx, n, st)
	case "select":
		var r struct {
			Error   string   `json:"error"`
			Options []string `json:"options"`
			Text    string   `json:"text"`
		}
		if err := c.callOn(ctx, n.obj, jsSelect, []any{st.Option}, &r); err != nil {
			return "", err
		}
		switch r.Error {
		case "":
		case "not a select":
			return "", &Error{Status: 422, Msg: n.ref + " isn't a <select>; click it, then click the option"}
		case "no option":
			opts := make([]string, 0, len(r.Options))
			for _, o := range r.Options[:min(len(r.Options), 10)] {
				opts = append(opts, quote(capText(o, maxField)))
			}
			return "", &Error{Status: 422, Msg: "no such option in " + n.ref + "; its first options: " + fence.Wrap("page options", strings.Join(opts, "\n"))}
		default:
			return "", &Error{Status: 422, Msg: n.ref + ": " + r.Error}
		}
		c.note("Selected an option in " + n.ref)
		return "selected " + quote(capText(r.Text, maxField)) + " in " + n.echo(), nil
	case "check", "uncheck":
		return c.check(ctx, n, st.Action == "check")
	case "upload":
		return c.upload(ctx, n, st.Files)
	}
	return "", &Error{Status: 400, Msg: "unknown action " + st.Action}
}

// aim scrolls a node into view and finds the point to act at: its
// centre, if nothing covers it there.
func (c *call) aim(ctx context.Context, n *node) (x, y float64, err error) {
	if err := c.renderer(ctx, c.t.quick, "DOM.scrollIntoViewIfNeeded", map[string]any{"backendNodeId": n.backend}, nil); err != nil {
		var ce *cdp.Error
		if errors.As(err, &ce) {
			return 0, 0, &Error{Status: 422, Msg: n.ref + " can't be scrolled into view (it may be hidden)"}
		}
		return 0, 0, err
	}
	var q struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := c.renderer(ctx, c.t.quick, "DOM.getContentQuads", map[string]any{"backendNodeId": n.backend}, &q); err != nil {
		var ce *cdp.Error
		if !errors.As(err, &ce) {
			return 0, 0, err
		}
	}
	for _, quad := range q.Quads {
		if len(quad) != 8 {
			continue
		}
		x, y = (quad[0]+quad[2]+quad[4]+quad[6])/4, (quad[1]+quad[3]+quad[5]+quad[7])/4
		if area(quad) >= 1 {
			break
		}
	}
	if x == 0 && y == 0 {
		return 0, 0, &Error{Status: 422, Msg: n.ref + " has no box on the page (it may be hidden or empty)"}
	}
	r, err := c.callOnObject(ctx, n.obj, jsHitTest, []any{x, y}, false)
	if err != nil {
		return 0, 0, err
	}
	switch {
	case r.Type == "boolean" && string(r.Value) == "true":
		return x, y, nil
	case r.Type == "object" && r.ObjectID != "":
		return 0, 0, &Error{Status: http.StatusConflict, Msg: n.ref + " is covered by " + c.describe(ctx, r.ObjectID)}
	}
	return 0, 0, &Error{Status: 422, Msg: n.ref + " isn't on screen at its centre"}
}

func area(q []float64) float64 {
	a := 0.0
	for i := 0; i < 4; i++ {
		j := (i + 1) % 4
		a += q[2*i]*q[2*j+1] - q[2*j]*q[2*i+1]
	}
	if a < 0 {
		a = -a
	}
	return a / 2
}

// letGo is a context for finishing an event pair (a button or key's
// release) even when ctx has been cancelled.
func (c *call) letGo(ctx context.Context) (context.Context, context.CancelFunc) {
	// Longer than a renderer call's own timeout, so a page that stops
	// answering is still told apart from a cancel.
	return context.WithTimeout(context.WithoutCancel(ctx), c.t.quick+time.Second)
}

// mouse dispatches one mouse event.
func (c *call) mouse(ctx context.Context, typ string, x, y float64, button string, clicks int) error {
	p := map[string]any{"type": typ, "x": x, "y": y}
	if button != "" {
		p["button"] = button
		p["clickCount"] = clicks
	}
	return c.renderer(ctx, c.t.quick, "Input.dispatchMouseEvent", p, nil)
}

// click moves the mouse to n's point (x, y), which aim checked, and
// clicks. At human pace the pointer glides to a point scattered around
// it (glide), pauses, and holds the button down for a moment. A press is
// always followed by its release, even if the call is cancelled in
// between.
func (c *call) click(ctx context.Context, n *node, x, y float64, button string, double bool) error {
	x, y, err := c.glide(ctx, n, x, y)
	if err != nil {
		return err
	}
	if !c.instant {
		if err := c.t.pause(ctx, c.st.mot.Dwell()); err != nil {
			return err
		}
		// The pointer took a while to get there: check nothing has
		// moved over the point meanwhile.
		if err := c.stillThere(ctx, n, x, y); err != nil {
			return err
		}
	}
	clicks := 1
	if double {
		clicks = 2
	}
	for i := 1; i <= clicks; i++ {
		if i > 1 && !c.instant {
			if err := c.t.pause(ctx, c.st.mot.JitterDuration(0.09, 0.02)); err != nil {
				return err
			}
		}
		// Once a press is sent it may have landed, even if its answer
		// never came (a cancel, a timeout): the release goes out anyway.
		// One that was never sent needs none.
		err := c.mouse(ctx, "mousePressed", x, y, button, i)
		if errors.Is(err, cdp.ErrNotSent) {
			return err
		}
		if err == nil && !c.instant {
			// A cancel cuts the hold short; the release still goes out.
			_ = c.t.pause(ctx, c.st.mot.KeyHold())
		}
		rctx, cancel := c.letGo(ctx)
		rerr := c.mouse(rctx, "mouseReleased", x, y, button, i)
		cancel()
		switch {
		case err != nil:
			return err
		case rerr != nil:
			return rerr
		case ctx.Err() != nil:
			return ctx.Err()
		}
	}
	return nil
}

// glide moves the pointer to n's point (x, y). At instant pace that is
// one move to it. At human pace, the pointer travels a hand's path
// (internal/motion) from where it last stopped, or in from an edge of
// the viewport, to a point scattered around (x, y) within n's box; it
// returns the point it reached.
func (c *call) glide(ctx context.Context, n *node, x, y float64) (float64, float64, error) {
	if c.instant {
		c.st.pointer = &motion.Point{X: x, Y: y}
		return x, y, c.mouse(ctx, "mouseMoved", x, y, "", 0)
	}
	to, size, err := c.scatter(ctx, n, x, y)
	if err != nil {
		return 0, 0, err
	}
	from := c.st.pointer
	if from == nil {
		vw, vh := 1280.0, 800.0
		if pi, err := c.info(ctx); err == nil && pi.W > 0 && pi.H > 0 {
			vw, vh = pi.W, pi.H
		}
		p := c.st.mot.Entry(motion.Size{W: vw, H: vh})
		from = &p
	}
	// The moves keep to the path's clock: a slow answer from the page
	// eats into the next pause rather than stretching the glide.
	start, due := time.Now(), time.Duration(0)
	for _, s := range c.st.mot.Path(*from, to, size) {
		if err := c.mouse(ctx, "mouseMoved", s.X, s.Y, "", 0); err != nil {
			return 0, 0, err
		}
		c.st.pointer = &motion.Point{X: s.X, Y: s.Y}
		due += s.Pause
		if err := c.t.pause(ctx, due-time.Since(start)); err != nil {
			return 0, 0, err
		}
	}
	return to.X, to.Y, nil
}

// scatter is where a hand aiming at n's point (x, y) lands: normally
// scattered around it within the box aim took it from (motion.Aim), if
// nothing covers n there; else (x, y) itself, which aim checked. size is
// the box's smaller side, for Fitts's law.
func (c *call) scatter(ctx context.Context, n *node, x, y float64) (motion.Point, float64, error) {
	at := motion.Point{X: x, Y: y}
	var q struct {
		Quads [][]float64 `json:"quads"`
	}
	if err := c.renderer(ctx, c.t.quick, "DOM.getContentQuads", map[string]any{"backendNodeId": n.backend}, &q); err != nil {
		var ce *cdp.Error
		if errors.As(err, &ce) {
			return at, 0, nil
		}
		return at, 0, err
	}
	for _, quad := range q.Quads {
		if len(quad) != 8 {
			continue
		}
		cx, cy := (quad[0]+quad[2]+quad[4]+quad[6])/4, (quad[1]+quad[3]+quad[5]+quad[7])/4
		if math.Abs(cx-x) > 0.5 || math.Abs(cy-y) > 0.5 {
			continue
		}
		lox, hix := min(quad[0], quad[2], quad[4], quad[6]), max(quad[0], quad[2], quad[4], quad[6])
		loy, hiy := min(quad[1], quad[3], quad[5], quad[7]), max(quad[1], quad[3], quad[5], quad[7])
		w, h := hix-lox, hiy-loy
		p := c.st.mot.Aim(at, motion.Size{W: w, H: h})
		r, err := c.callOnObject(ctx, n.obj, jsHitTest, []any{p.X, p.Y}, false)
		if err != nil {
			return at, 0, err
		}
		if r.Type != "boolean" || string(r.Value) != "true" {
			return at, min(w, h), nil // a rounded corner, a cut-out: keep to the middle
		}
		return p, min(w, h), nil
	}
	return at, 0, nil
}

// stillThere checks nothing covers n at (x, y) (jsHitTest), as aim does
// before a pointer step.
func (c *call) stillThere(ctx context.Context, n *node, x, y float64) error {
	r, err := c.callOnObject(ctx, n.obj, jsHitTest, []any{x, y}, false)
	if err != nil {
		return err
	}
	switch {
	case r.Type == "boolean" && string(r.Value) == "true":
		return nil
	case r.Type == "object" && r.ObjectID != "":
		return &Error{Status: http.StatusConflict, Msg: n.ref + " is covered by " + c.describe(ctx, r.ObjectID)}
	}
	return &Error{Status: http.StatusConflict, Msg: n.ref + " moved while the pointer went to it; take a new snapshot"}
}

// key dispatches one key event.
func (c *call) key(ctx context.Context, typ string, k keyDef, mods int, commands ...string) error {
	p := map[string]any{"type": typ, "key": k.key, "code": k.code, "windowsVirtualKeyCode": k.vk, "modifiers": mods}
	if typ == "char" {
		p["text"], p["unmodifiedText"] = k.text, k.text
	}
	if len(commands) > 0 {
		p["commands"] = commands
	}
	return c.renderer(ctx, c.t.quick, "Input.dispatchKeyEvent", p, nil)
}

// pressCombo presses a key with its modifiers, as a keyboard sends it:
// raw key-downs, a char if it types one, then the key-ups. Whatever was
// pressed is released, even if the call is cancelled.
//
// At human pace, the key waits for the gap since the last one (keyDue,
// a pause between combinations unless typeInto set a typist's) and is
// held for a moment before the key-ups.
func (c *call) pressCombo(ctx context.Context, cb combo, commands ...string) error {
	var down []keyDef
	mods := 0
	if !c.instant {
		if !c.keyAt.IsZero() {
			if err := c.t.pause(ctx, c.keyDue-time.Since(c.keyAt)); err != nil {
				return err
			}
		}
		c.keyAt, c.keyDue = time.Now(), c.st.mot.JitterDuration(0.18, 0.06)
	}
	defer func() {
		if !c.instant && len(down) > 0 {
			// A cancel cuts the hold short; the key-ups still go out.
			_ = c.t.pause(ctx, c.st.mot.KeyHold())
		}
		rctx, cancel := c.letGo(ctx)
		defer cancel()
		for i := len(down) - 1; i >= 0; i-- {
			if down[i] == cb.key {
				_ = c.key(rctx, "keyUp", down[i], mods)
				continue
			}
			mods &^= modBit(down[i])
			_ = c.key(rctx, "keyUp", down[i], mods)
		}
	}()
	// press sends a key-down. The key is noted as down unless it was never
	// sent: it may land even if the answer never comes, and the deferred
	// release covers it.
	press := func(k keyDef, mods int, commands ...string) error {
		err := c.key(ctx, "rawKeyDown", k, mods, commands...)
		if !errors.Is(err, cdp.ErrNotSent) {
			down = append(down, k)
		}
		return err
	}
	shiftDown := false
	for _, m := range cb.mods {
		mods |= modBit(m)
		if m.key == "Shift" {
			shiftDown = true
		}
		if err := press(m, mods); err != nil {
			return err
		}
	}
	if cb.key.shift && !shiftDown {
		sh := modifierKeys["shift"].def
		mods |= modShift
		if err := press(sh, mods); err != nil {
			return err
		}
	}
	if err := press(cb.key, mods, commands...); err != nil {
		return err
	}
	if cb.key.text != "" && mods&(modCtrl|modAlt|modMeta) == 0 && len(commands) == 0 {
		if err := c.key(ctx, "char", cb.key, mods); err != nil {
			return err
		}
	}
	return nil
}

func modBit(k keyDef) int {
	switch k.key {
	case "Control":
		return modCtrl
	case "Alt":
		return modAlt
	case "Meta":
		return modMeta
	case "Shift":
		return modShift
	}
	return 0
}

// selectAll is ctrl+a, sent with Chromium's selectAll editing command.
var selectAll = combo{mods: []keyDef{modifierKeys["ctrl"].def}, key: usKeys['a'], name: "ctrl+a"}

// unsupportedTypes are inputs act doesn't type into.
var unsupportedTypes = map[string]string{
	"date": "a date", "datetime-local": "a date", "month": "a date", "week": "a date", "time": "a time",
	"range": "a range", "color": "a colour", "file": "a file",
	"checkbox": "a checkbox (use check)", "radio": "a radio button (use check)",
	"submit": "a button", "button": "a button", "reset": "a button", "image": "a button", "hidden": "a hidden",
}

type fieldInfo struct {
	Tag      string `json:"tag"`
	Type     string `json:"type"`
	Editable bool   `json:"editable"`
	Disabled bool   `json:"disabled"`
	Checked  *bool  `json:"checked"`
	Aria     string `json:"aria"`
	Role     string `json:"role"`
	File     bool   `json:"file"` // a file input, or a label for one
}

// typeInto fills or types into a field: focus it, check it has the
// focus, put the text in, and check the focus didn't move.
func (c *call) typeInto(ctx context.Context, n *node, st Step) (string, error) {
	var f fieldInfo
	if err := c.callOn(ctx, n.obj, jsField, nil, &f); err != nil {
		return "", err
	}
	switch {
	case f.Tag == "select":
		return "", &Error{Status: 422, Msg: n.ref + " is a select; use select"}
	case f.Tag == "input" && unsupportedTypes[f.Type] != "":
		what := unsupportedTypes[f.Type]
		msg := fmt.Sprintf("act doesn't type into %s input (%s)", what, n.ref)
		if f.Type == "file" {
			msg += "; put the file in the session with upload_file, then use an upload step"
		}
		return "", &Error{Status: 422, Msg: msg}
	case f.Tag != "input" && f.Tag != "textarea" && !f.Editable:
		return "", &Error{Status: 422, Msg: n.ref + " isn't a text field"}
	case f.Disabled:
		return "", &Error{Status: 422, Msg: n.ref + " is disabled"}
	}
	if err := c.focus(ctx, n); err != nil {
		return "", err
	}
	text := *st.Text
	count := utf8.RuneCountInString(text)
	if st.Action == "fill" {
		// Replace what's there: select all, then put the text in at once.
		if err := c.pressCombo(ctx, selectAll, "selectAll"); err != nil {
			return "", err
		}
		if text == "" {
			if err := c.pressCombo(ctx, combo{key: namedKeys["backspace"]}); err != nil {
				return "", err
			}
		} else if err := c.renderer(ctx, c.t.quick, "Input.insertText", map[string]any{"text": text}, nil); err != nil {
			return "", err
		}
		if err := c.stillFocused(ctx, n, count); err != nil {
			return "", err
		}
		c.note(fmt.Sprintf("Filled %s (%s)", n.ref, characters(count)))
		return fmt.Sprintf("filled %s (typed %s)", n.echo(), characters(count)), nil
	}
	typed := 0
	runes := []rune(text)
	for i, r := range runes {
		if k, ok := usKeys[r]; ok {
			if err := c.pressCombo(ctx, combo{key: k}); err != nil {
				return "", err
			}
		} else {
			if !c.instant && !c.keyAt.IsZero() {
				if err := c.t.pause(ctx, c.keyDue-time.Since(c.keyAt)); err != nil {
					return "", err
				}
			}
			c.keyAt = time.Now()
			if err := c.renderer(ctx, c.t.quick, "Input.insertText", map[string]any{"text": string(r)}, nil); err != nil {
				return "", err
			}
		}
		if !c.instant {
			// A typist's gap before the next key (Dhakal et al. 2018).
			next := rune(0)
			if i+1 < len(runes) {
				next = runes[i+1]
			}
			c.keyDue = c.st.mot.KeyDelay(r, next)
		}
		typed++
		if err := c.stillFocused(ctx, n, typed); err != nil {
			return "", err
		}
	}
	c.note(fmt.Sprintf("Typed %s into %s", characters(count), n.ref))
	return fmt.Sprintf("typed %s into %s", characters(count), n.echo()), nil
}

// upload sets files from the session's uploads folder on a file input,
// or on the file input a label controls, with DOM.setFileInputFiles
// (PROJ-117). Chromium then fires input and change, as when a person
// picks the files in the chooser; no chooser opens. Each name must be a
// regular file in uploads (files.Store.Uploaded, through os.Root), and
// only the /files/uploads/<name> paths it gives reach the browser.
func (c *call) upload(ctx context.Context, n *node, names []string) (string, error) {
	if c.t.files == nil {
		return "", &Error{Status: 422, Msg: "uploads aren't available on this server"}
	}
	paths := make([]string, 0, len(names))
	for _, name := range names {
		_, p, err := c.t.files.Uploaded(c.sess.Profile, name)
		switch {
		case errors.Is(err, files.ErrNotFound), errors.Is(err, files.ErrBadName):
			return "", &Error{Status: 422, Msg: "no uploaded file named " + strconv.Quote(name) + "; upload it first with upload_file"}
		case err != nil:
			return "", err
		}
		if !strings.HasPrefix(p, files.MountPoint+"/"+files.Uploads+"/") {
			return "", fmt.Errorf("upload path %q isn't in the uploads folder", p) // can't happen
		}
		paths = append(paths, p)
	}
	r, err := c.callOnObject(ctx, n.obj, jsFileInput, nil, false)
	if err != nil {
		return "", err
	}
	if r.ObjectID == "" {
		return "", &Error{Status: 422, Msg: n.ref + " isn't a file input (<input type=file>) or a label for one. A page's own button that opens a file chooser can't be handled: the chooser waits for a person. " +
			"Target the <input type=file> itself, which is often styled out of sight; if the snapshot doesn't list it, target the <label> that controls it (find its text); otherwise request a file_upload handoff."}
	}
	var in struct {
		Multiple bool `json:"multiple"`
		Disabled bool `json:"disabled"`
		Count    int  `json:"count"`
	}
	if err := c.callOn(ctx, r.ObjectID, jsFileInfo, nil, &in); err != nil {
		return "", err
	}
	switch {
	case in.Disabled:
		return "", &Error{Status: 422, Msg: n.ref + "'s file input is disabled"}
	case len(paths) > 1 && !in.Multiple:
		return "", &Error{Status: 422, Msg: fmt.Sprintf("%s takes one file (its input has no multiple attribute), not %d", n.ref, len(paths))}
	}
	var d struct {
		Node struct {
			BackendNodeID int64 `json:"backendNodeId"`
		} `json:"node"`
	}
	if err := c.renderer(ctx, c.t.quick, "DOM.describeNode", map[string]any{"objectId": r.ObjectID}, &d); err != nil {
		return "", err
	}
	if d.Node.BackendNodeID == 0 {
		return "", errStale(n.ref)
	}
	if err := c.renderer(ctx, c.t.quick, "DOM.setFileInputFiles", map[string]any{"files": paths, "backendNodeId": d.Node.BackendNodeID}, nil); err != nil {
		return "", err
	}
	if err := c.callOn(ctx, r.ObjectID, jsFileInfo, nil, &in); err != nil {
		return "", err
	}
	if in.Count != len(paths) {
		return "", &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("set %s on %s, but its input holds %d", fileCount(len(paths)), n.ref, in.Count)}
	}
	// The names are the agent's own, not the page's; the UI escapes them.
	c.note("Uploaded " + strings.Join(names, ", ") + " to " + n.ref)
	return "uploaded " + fileCount(len(paths)) + " to " + n.echo(), nil
}

// fileCount counts files.
func fileCount(n int) string {
	if n == 1 {
		return "1 file"
	}
	return fmt.Sprintf("%d files", n)
}

func characters(n int) string {
	if n == 1 {
		return "1 character"
	}
	return fmt.Sprintf("%d characters", n)
}

// focus focuses a node and checks it is the active element.
func (c *call) focus(ctx context.Context, n *node) error {
	if err := c.renderer(ctx, c.t.quick, "DOM.focus", map[string]any{"backendNodeId": n.backend}, nil); err != nil {
		var ce *cdp.Error
		if errors.As(err, &ce) {
			return &Error{Status: 422, Msg: n.ref + " can't take the focus"}
		}
		return err
	}
	var ok bool
	if err := c.callOn(ctx, n.obj, jsFocused, nil, &ok); err != nil {
		return err
	}
	if !ok {
		return &Error{Status: http.StatusConflict, Msg: n.ref + " didn't take the focus; " + c.focusHolder(ctx, n)}
	}
	return nil
}

// stillFocused checks the node kept the focus while typing.
func (c *call) stillFocused(ctx context.Context, n *node, typed int) error {
	var ok bool
	if err := c.callOn(ctx, n.obj, jsFocused, nil, &ok); err != nil {
		return err
	}
	if !ok {
		return &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("the focus left %s after %s; %s", n.ref, characters(typed), c.focusHolder(ctx, n))}
	}
	return nil
}

// focusHolder says what has the focus instead.
func (c *call) focusHolder(ctx context.Context, n *node) string {
	r, err := c.callOnObject(ctx, n.obj, jsActive, nil, false)
	if err != nil || r.ObjectID == "" {
		return "nothing has it now"
	}
	return "it went to " + c.describe(ctx, r.ObjectID)
}

// check checks or unchecks a checkbox, radio button or switch by
// clicking it (or its label), unless it already is, and checks it took.
func (c *call) check(ctx context.Context, n *node, want bool) (string, error) {
	state := func() (bool, bool, error) {
		var f fieldInfo
		if err := c.callOn(ctx, n.obj, jsField, nil, &f); err != nil {
			return false, false, err
		}
		radio := f.Type == "radio" || f.Role == "radio" || f.Role == "menuitemradio"
		if f.Checked != nil && f.Tag == "input" && (f.Type == "checkbox" || f.Type == "radio") {
			return *f.Checked, radio, nil
		}
		if f.Aria == "true" || f.Aria == "false" || f.Aria == "mixed" {
			return f.Aria == "true", radio, nil
		}
		return false, radio, &Error{Status: 422, Msg: n.ref + " isn't a checkbox, radio button or switch"}
	}
	verb := map[bool]string{true: "checked", false: "unchecked"}[want]
	now, radio, err := state()
	if err != nil {
		return "", err
	}
	if radio && !want {
		return "", &Error{Status: 422, Msg: "a radio button can't be unchecked; check another one"}
	}
	if now == want {
		return n.echo() + " was already " + verb, nil
	}
	x, y, err := c.aim(ctx, n)
	if err != nil {
		return "", err
	}
	if err := c.click(ctx, n, x, y, "left", false); err != nil {
		return "", err
	}
	if after, _, err := state(); err != nil {
		return "", err
	} else if after != want {
		return "", &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("clicked %s but it is still %s", n.ref, map[bool]string{true: "checked", false: "unchecked"}[after])}
	}
	c.note(strings.ToUpper(verb[:1]) + verb[1:] + " " + n.ref)
	return verb + " " + n.echo(), nil
}
