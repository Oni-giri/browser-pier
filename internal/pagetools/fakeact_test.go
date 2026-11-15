package pagetools

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/cdp/cdptest"
)

// What the fake's elements do when acted on.
type behaviour struct {
	cover     *fnode // what's on top of it at its centre
	field     string // an input's type ("text", "date", "checkbox"…); "" for not a field
	checked   bool
	options   []string // a select's options
	selected  string
	onClick   func()
	stealsTo  *fnode // focus moves here after the first character
	unfocused bool   // refuses the focus
	// A file input's multiple and disabled, and the files set on it;
	// control is the input a <label> controls.
	multiple, disabled bool
	files              []string
	control            *fnode
}

// tabState of the fake: what has the focus, what was aimed at, and the
// input events, in order.
type fakeInput struct {
	focused  *fnode
	selected bool // everything in the focused field is selected
	aimed    *fnode
	events   []string
	mouse    []mouseEvent // the mouse events, with where they were
}

type mouseEvent struct {
	typ  string
	x, y float64
}

func (fb *fakeBrowser) node(tb *ftab, backend int64) *fnode {
	var found *fnode
	var walk func(n *fnode)
	walk = func(n *fnode) {
		if n.backend == backend {
			found = n
		}
		for _, k := range n.kids {
			walk(k)
		}
	}
	walk(tb.doc)
	return found
}

func objID(n *fnode) string { return "obj-" + strconv.FormatInt(n.backend, 10) }
func objBackend(id string) int64 {
	b, _ := strconv.ParseInt(strings.TrimPrefix(id, "obj-"), 10, 64)
	return b
}
func value(v any) map[string]any {
	return map[string]any{"result": map[string]any{"type": "object", "value": v}}
}
func nodeObj(n *fnode) map[string]any {
	return map[string]any{"result": map[string]any{"type": "object", "subtype": "node", "objectId": objID(n)}}
}

// dom answers the DOM, Accessibility and Input methods act uses.
func (fb *fakeBrowser) dom(tb *ftab, m cdptest.Message, p map[string]any) (any, bool, error) {
	in := fb.input(tb)
	backend := func() *fnode {
		if b, ok := p["backendNodeId"].(float64); ok {
			return fb.node(tb, int64(b))
		}
		if id, ok := p["objectId"].(string); ok {
			return fb.node(tb, objBackend(id))
		}
		return nil
	}
	gone := cdptest.Error{Message: "No node with given id found"}
	switch m.Method {
	case "DOM.resolveNode":
		n := backend()
		if n == nil {
			return nil, true, gone
		}
		return map[string]any{"object": map[string]any{"type": "object", "subtype": "node", "objectId": objID(n)}}, true, nil
	case "DOM.describeNode":
		n := backend()
		if n == nil {
			return nil, true, gone
		}
		return map[string]any{"node": map[string]any{"backendNodeId": n.backend, "nodeName": strings.ToUpper(n.tag), "attributes": append([]string{}, n.attrs...)}}, true, nil
	case "Accessibility.getPartialAXTree":
		n := backend()
		if n == nil {
			return nil, true, gone
		}
		return map[string]any{"nodes": []any{map[string]any{"nodeId": "1", "ignored": n.role == "", "role": map[string]any{"value": n.role}, "name": map[string]any{"value": n.name}}}}, true, nil
	case "DOM.scrollIntoViewIfNeeded":
		if backend() == nil {
			return nil, true, gone
		}
		return nil, true, nil
	case "DOM.getContentQuads":
		n := backend()
		if n == nil || n.box == nil {
			return nil, true, cdptest.Error{Message: "Could not compute content quads."}
		}
		b := *n.box
		in.aimed = n
		return map[string]any{"quads": [][]float64{{b[0], b[1], b[0] + b[2], b[1], b[0] + b[2], b[1] + b[3], b[0], b[1] + b[3]}}}, true, nil
	case "DOM.focus":
		n := backend()
		if n == nil {
			return nil, true, gone
		}
		if bh := fb.behave[n]; bh == nil || !bh.unfocused {
			in.focused, in.selected = n, false
		}
		return nil, true, nil
	case "Runtime.callFunctionOn":
		n := fb.node(tb, objBackend(p["objectId"].(string)))
		if n == nil {
			return nil, true, cdptest.Error{Message: "Could not find object with given id"}
		}
		if fn, _ := p["functionDeclaration"].(string); !strings.Contains(fn, "/*bp:") {
			// The helper's check that the node is in the world: the
			// world's global must still be this document's.
			as, _ := p["arguments"].([]any)
			if len(as) != 1 {
				return nil, true, cdptest.Error{Message: "fake: unexpected function " + fn}
			}
			g, _ := as[0].(map[string]any)["objectId"].(string)
			if !strings.Contains(g, "/"+tb.id+"/"+tb.loader) || strings.HasSuffix(g, "/main") {
				return nil, true, cdptest.Error{Message: "Argument should belong to the same JavaScript world as target object"}
			}
			return value(true), true, nil
		}
		return fb.function(tb, n, p)
	case "Input.dispatchMouseEvent":
		typ := p["type"].(string)
		at := ""
		if in.aimed != nil {
			at = in.aimed.name
		}
		in.events = append(in.events, typ+" "+at)
		x, _ := p["x"].(float64)
		y, _ := p["y"].(float64)
		in.mouse = append(in.mouse, mouseEvent{typ, x, y})
		if fb.onInput != nil {
			fb.onInput(typ)
		}
		if typ == "mouseReleased" && in.aimed != nil {
			fb.clickOn(tb, in.aimed)
		}
		return nil, true, nil
	case "Input.dispatchKeyEvent":
		typ := p["type"].(string)
		key, _ := p["key"].(string)
		in.events = append(in.events, typ+" "+key)
		if fb.onInput != nil {
			fb.onInput(typ)
		}
		if cmds, ok := p["commands"].([]any); ok && len(cmds) > 0 && cmds[0] == "selectAll" {
			in.selected = true
		}
		switch {
		case typ == "char":
			fb.typeText(tb, p["text"].(string))
		case typ == "rawKeyDown" && key == "Backspace" && in.focused != nil && in.focused.value != nil:
			if in.selected {
				*in.focused.value = ""
			}
		}
		return nil, true, nil
	case "Input.insertText":
		in.events = append(in.events, "insertText")
		fb.typeText(tb, p["text"].(string))
		return nil, true, nil
	case "DOM.setFileInputFiles":
		n := backend()
		if n == nil {
			return nil, true, gone
		}
		bh := fb.behave[n]
		if bh == nil || bh.field != "file" {
			return nil, true, cdptest.Error{Message: "Node is not a file input element"}
		}
		bh.files = nil
		fs, _ := p["files"].([]any)
		for _, f := range fs {
			bh.files = append(bh.files, f.(string))
		}
		in.events = append(in.events, "setFileInputFiles "+n.name)
		return nil, true, nil
	}
	return nil, false, nil
}

func (fb *fakeBrowser) typeText(tb *ftab, s string) {
	in := fb.input(tb)
	f := in.focused
	if f == nil {
		return
	}
	if f.value == nil {
		f.value = new("")
	}
	if in.selected {
		*f.value, in.selected = "", false
	}
	*f.value += s
	if bh := fb.behave[f]; bh != nil && bh.stealsTo != nil {
		in.focused = bh.stealsTo
	}
}

func (fb *fakeBrowser) clickOn(tb *ftab, n *fnode) {
	bh := fb.behave[n]
	if bh == nil {
		return
	}
	if bh.field == "checkbox" {
		bh.checked = !bh.checked
	}
	if bh.onClick != nil {
		bh.onClick()
	}
}

func (fb *fakeBrowser) input(tb *ftab) *fakeInput {
	if fb.inputs == nil {
		fb.inputs = map[*ftab]*fakeInput{}
	}
	if fb.inputs[tb] == nil {
		fb.inputs[tb] = &fakeInput{}
	}
	return fb.inputs[tb]
}

// function runs one of act's isolated-world scripts, by its marker.
func (fb *fakeBrowser) function(tb *ftab, n *fnode, p map[string]any) (any, bool, error) {
	fn := p["functionDeclaration"].(string)
	var args []any
	if as, ok := p["arguments"].([]any); ok {
		for _, a := range as {
			args = append(args, a.(map[string]any)["value"])
		}
	}
	bh := fb.behave[n]
	if bh == nil {
		bh = &behaviour{}
	}
	in := fb.input(tb)
	marker := fn[strings.Index(fn, "/*bp:")+5 : strings.Index(fn, "*/")]
	switch marker {
	case "connected":
		return value(!fb.removed[n]), true, nil
	case "hittest":
		if bh.cover != nil {
			return nodeObj(bh.cover), true, nil
		}
		return map[string]any{"result": map[string]any{"type": "boolean", "value": true}}, true, nil
	case "focused":
		return value(in.focused == n), true, nil
	case "active":
		if in.focused == nil {
			return map[string]any{"result": map[string]any{"type": "object", "subtype": "null", "value": nil}}, true, nil
		}
		return nodeObj(in.focused), true, nil
	case "field":
		var checked any
		if bh.field == "checkbox" || bh.field == "radio" {
			checked = bh.checked
		}
		tag := n.tag
		typ := bh.field
		if len(bh.options) > 0 {
			tag, typ = "select", ""
		}
		return value(map[string]any{"tag": tag, "type": typ, "editable": false, "disabled": false, "checked": checked, "file": typ == "file"}), true, nil
	case "select":
		want := args[0].(string)
		for _, o := range bh.options {
			if o == want {
				bh.selected = o
				return value(map[string]any{"text": o}), true, nil
			}
		}
		return value(map[string]any{"error": "no option", "options": bh.options}), true, nil
	case "fileinput":
		if bh.field == "file" {
			return nodeObj(n), true, nil
		}
		if c := bh.control; n.tag == "label" && c != nil && fb.behave[c] != nil && fb.behave[c].field == "file" {
			return nodeObj(c), true, nil
		}
		return map[string]any{"result": map[string]any{"type": "object", "subtype": "null", "value": nil}}, true, nil
	case "fileinfo":
		return value(map[string]any{"multiple": bh.multiple, "disabled": bh.disabled, "count": len(bh.files)}), true, nil
	case "visible":
		return value(!fb.removed[n] && n.box != nil && (*n.box)[2] > 0), true, nil
	}
	return nil, true, cdptest.Error{Message: "fake: unknown function " + marker}
}

var _ = json.Marshal
