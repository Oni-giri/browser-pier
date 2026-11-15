package pagetools

import (
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// targetPage is a page for role and text targets: buttons, two of them
// named alike, fields (one a password), a paragraph with a link, and
// copies hidden in each way a snapshot leaves out.
type targetPage struct {
	doc                       *fnode
	save, cancel, del1, del2  *fnode
	email, pw, para, link     *fnode
	hiddenSave, invisibleSave *fnode
	mu                        sync.Mutex
	clicks                    []string
}

func newTargetPage(fb *fakeBrowser) *targetPage {
	p := &targetPage{}
	y := 10.0
	next := func() *[4]float64 { y += 40; return box(10, y, 200, 30) }
	button := func(name, label string) *fnode {
		b := next()
		n := el("button", "button", name, b, txt(name, b))
		fb.behave[n] = &behaviour{onClick: func() { p.mu.Lock(); p.clicks = append(p.clicks, label); p.mu.Unlock() }}
		return n
	}
	field := func(name, value string, attrs ...string) *fnode {
		n := el("input", "textbox", name, next())
		n.value = new(value)
		for i := 0; i+1 < len(attrs); i += 2 {
			n.attr(attrs[i], attrs[i+1])
		}
		fb.behave[n] = &behaviour{field: "text"}
		return n
	}
	p.save = button("Save", "save")
	p.cancel = button("Cancel", "cancel")
	p.del1 = button("Delete", "delete 1")
	p.del2 = button("Delete", "delete 2")
	// A display:none copy and a visibility:hidden one: neither counts.
	p.hiddenSave = button("Save", "hidden save")
	p.hiddenSave.display = "none"
	p.invisibleSave = button("Save", "invisible save")
	p.invisibleSave.visib = "hidden"
	p.email = field("Email", "ana@example.com", "name", "email")
	p.pw = field("Password", "hunter2-SECRET", "type", "password")
	b := next()
	p.link = el("a", "link", "here", b, txt("here", b)).with(func(n *fnode) { n.display = "inline" })
	fb.behave[p.link] = &behaviour{onClick: func() { p.mu.Lock(); p.clicks = append(p.clicks, "link"); p.mu.Unlock() }}
	p.para = el("p", "paragraph", "", b, txt("Save all changes, or click ", b), p.link, txt(" to leave", b))
	fb.behave[p.para] = &behaviour{onClick: func() { p.mu.Lock(); p.clicks = append(p.clicks, "paragraph"); p.mu.Unlock() }}
	p.doc = el("#document", "RootWebArea", "", box(0, 0, 1280, 800), p.save, p.cancel, p.del1, p.del2, p.hiddenSave, p.invisibleSave, p.email, p.pw, p.para)
	return p
}

func (p *targetPage) clicked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.clicks...)
}

var candidateRef = regexp.MustCompile(`\[ref=([a-z][a-z0-9]e[0-9]+)\]`)

// candidateRefs are the refs an error hands out, in order.
func candidateRefs(msg string) []string {
	var out []string
	for _, m := range candidateRef.FindAllStringSubmatch(msg, -1) {
		out = append(out, m[1])
	}
	return out
}

// TestActOnATarget: a role target that matches one visible element is
// acted on with no snapshot first, and the result names it with a ref
// that works like a snapshot's.
func TestActOnATarget(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	out, err := f.act(
		Step{Action: "fill", Target: `role=textbox[name="email"]`, Text: text("bo@example.com")},
		Step{Action: "fill", Target: `role=textbox[name='PASSWORD']`, Text: text("new-SECRET")},
		Step{Action: "click", Target: `role=button[name="save"]`},
	)
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`clicked ([a-z][a-z0-9]e[0-9]+) button "Save"`).FindStringSubmatch(out)
	if m == nil || !strings.Contains(out, `textbox "Email" (typed 14 characters)`) || !strings.Contains(out, `textbox "Password" (typed 10 characters)`) {
		t.Fatalf("act:\n%s", out)
	}
	if *p.email.value != "bo@example.com" || *p.pw.value != "new-SECRET" {
		t.Errorf("fields: %q %q", *p.email.value, *p.pw.value)
	}
	if strings.Contains(out, "SECRET") {
		t.Errorf("the password is in the answer:\n%s", out)
	}
	// The ref it named works in a following call, as a snapshot's would.
	if _, err := f.act(Step{Action: "click", Ref: m[1]}); err != nil {
		t.Fatal(err)
	}
	if got := p.clicked(); len(got) != 2 || got[0] != "save" || got[1] != "save" {
		t.Errorf("clicks %v", got)
	}
	checkSent(t, f.fb)
}

// TestAnAmbiguousTargetListsRefs: two visible matches fail with both,
// fenced, each with a ref that works in the next call; >> nth picks one.
func TestAnAmbiguousTargetListsRefs(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	_, err := f.act(Step{Action: "click", Target: `role=button[name="Delete"]`})
	if !isStatus(err, 409) {
		t.Fatalf("ambiguous: %v", err)
	}
	msg := err.Error()
	refs := candidateRefs(msg)
	if !strings.Contains(msg, `role=button[name="Delete"] matches 2 visible elements:`) || !strings.Contains(msg, "<untrusted-content-") ||
		strings.Count(msg, `button "Delete" [ref=`) != 2 || len(refs) != 2 || refs[0] == refs[1] || !strings.Contains(msg, ">> nth=0") {
		t.Fatalf("ambiguous:\n%s", msg)
	}
	if len(p.clicked()) != 0 {
		t.Errorf("clicked %v", p.clicked())
	}
	if _, err := f.act(Step{Action: "click", Ref: refs[1]}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.act(Step{Action: "click", Target: `role=button[name="Delete"] >> nth=0`}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.act(Step{Action: "click", Target: `role=button[name="Delete"] >> nth=-1`}); err != nil {
		t.Fatal(err)
	}
	if got := p.clicked(); strings.Join(got, ",") != "delete 2,delete 1,delete 2" {
		t.Errorf("clicks %v", got)
	}
	// nth past the matches says how many there are.
	_, err = f.act(Step{Action: "click", Target: `role=button[name="Delete"] >> nth=2`})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), `no visible element matches role=button[name="Delete"] >> nth=2: role=button[name="Delete"] matches 2 visible elements:`) {
		t.Errorf("nth past the end: %v", err)
	}
	// The step's failure is act's: later steps don't run.
	_, err = f.act(Step{Action: "click", Target: `role=button[name="Delete"]`}, Step{Action: "click", Target: `role=button[name="Save"]`})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "Step 1 (click) failed: ") || !strings.Contains(err.Error(), "The step after it didn't run.") {
		t.Errorf("a failing step: %v", err)
	}
	checkSent(t, f.fb)
}

// TestATargetWithNoMatch: none lists the visible elements of that role
// (at most 5, then how many more), or says there are none.
func TestATargetWithNoMatch(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	for _, name := range []string{"One", "Two", "Three"} {
		p.doc.kids = append(p.doc.kids, el("button", "button", name, box(400, 10, 50, 20)))
	}
	f.fb.addTab("https://example.com/", p.doc, true)
	_, err := f.act(Step{Action: "click", Target: `role=button[name="Publish"]`})
	if !isStatus(err, 409) {
		t.Fatalf("no match: %v", err)
	}
	msg := err.Error()
	// Save, Cancel, Delete, Delete, One, then 2 more; never the hidden Saves.
	if !strings.Contains(msg, `no visible element matches role=button[name="Publish"]; the page's visible button elements:`) ||
		!strings.Contains(msg, `button "Save" [ref=`) || !strings.Contains(msg, `button "One" [ref=`) || strings.Contains(msg, `"Two"`) ||
		!strings.HasSuffix(msg, "\nand 2 more") || len(candidateRefs(msg)) != 5 || !strings.Contains(msg, "<untrusted-content-") {
		t.Errorf("no match:\n%s", msg)
	}
	_, err = f.act(Step{Action: "check", Target: `role=checkbox`})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "the page has no visible checkbox element") {
		t.Errorf("no such role: %v", err)
	}
	_, err = f.act(Step{Action: "click", Target: `text=Publish`})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "no visible element matches text=Publish. ") {
		t.Errorf("no text: %v", err)
	}
	// A bad target is refused before any step runs.
	_, err = f.act(Step{Action: "click", Target: `role=button[name="Save"]`}, Step{Action: "click", Target: `#delete`})
	if !isStatus(err, 400) || !strings.Contains(err.Error(), "step 2: click: target ") || len(p.clicked()) != 0 {
		t.Errorf("bad target: %v %v", err, p.clicked())
	}
	_, err = f.act(Step{Action: "click", Ref: "a1e2", Target: `role=button`})
	if !isStatus(err, 400) || !strings.Contains(err.Error(), "not both") {
		t.Errorf("ref and target: %v", err)
	}
	_, err = f.act(Step{Action: "press", Keys: "Enter", Target: `role=button`})
	if !isStatus(err, 400) || !strings.Contains(err.Error(), "it takes no target") {
		t.Errorf("press with a target: %v", err)
	}
}

// TestHiddenCopiesDontCount: a display:none, visibility:hidden or
// aria-hidden copy, and one with no box, don't make a target ambiguous.
func TestHiddenCopiesDontCount(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	ignored := el("button", "button", "Save", box(500, 10, 50, 20)).with(func(n *fnode) { n.ignored = true })
	unboxed := el("button", "button", "Save", nil)
	zero := el("button", "button", "Save", box(500, 50, 0, 0))
	p.doc.kids = append(p.doc.kids, ignored, unboxed, zero)
	f.fb.addTab("https://example.com/", p.doc, true)
	if _, err := f.act(Step{Action: "click", Target: `role=button[name="Save"]`}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.act(Step{Action: "click", Target: `text="Save"`}); err != nil {
		t.Fatal(err)
	}
	if got := p.clicked(); strings.Join(got, ",") != "save,save" {
		t.Errorf("clicks %v", got)
	}
}

// TestTextTargets: text= is a part of the text, ignoring case; text="…"
// the whole text, exactly; either finds the innermost element holding it,
// never a field's value.
func TestTextTargets(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	// "save" is the button's text and in the paragraph's.
	_, err := f.act(Step{Action: "click", Target: `text=save`})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "text=save matches 2 visible elements:") ||
		!strings.Contains(err.Error(), `button "Save" [ref=`) || !strings.Contains(err.Error(), `paragraph "Save all changes, or click here to leave" [ref=`) {
		t.Fatalf("substring: %v", err)
	}
	for _, c := range []struct{ target, want string }{
		{`text="Save"`, "save"},
		{`text=here`, "link"},            // the link, not the paragraph around it
		{`text=CLICK HERE`, "paragraph"}, // across the link: the paragraph
		{`text="Save all changes, or click here to leave"`, "paragraph"},
		{`text=save >> nth=1`, "paragraph"},
	} {
		if _, err := f.act(Step{Action: "click", Target: c.target}); err != nil {
			t.Errorf("%s: %v", c.target, err)
			continue
		}
		if got := p.clicked(); got[len(got)-1] != c.want {
			t.Errorf("%s clicked %v", c.target, got)
		}
	}
	for _, target := range []string{`text="save"`, `text="Save all"`, `text=hunter2`, `text="hunter2-SECRET"`, `text=ana@example`} {
		_, err := f.act(Step{Action: "click", Target: target})
		// The target is echoed (the agent's own words), never the value.
		if !isStatus(err, 409) || !strings.Contains(err.Error(), "no visible element matches") || strings.Contains(strings.ReplaceAll(err.Error(), target, ""), "SECRET") {
			t.Errorf("%s: %v", target, err)
		}
	}
	// A field is named by its label; its value is never shown.
	_, err = f.act(Step{Action: "fill", Target: `role=textbox`, Text: text("x")})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), `textbox "Password" [ref=`) || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "ana@") {
		t.Errorf("fields: %v", err)
	}
}

// TestWaitForATarget: visible is one visible match, whose ref the answer
// gives; hidden is none; several fail after ambiguousFor with act's error.
func TestWaitForATarget(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	f.fb.mu.Lock()
	p.save.box = nil
	f.fb.mu.Unlock()
	f.fb.addTab("https://example.com/", p.doc, true)
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.fb.mu.Lock()
		p.save.box = box(10, 50, 200, 30)
		f.fb.mu.Unlock()
	}()
	out, err := f.waitIn(WaitIn{Target: `role=button[name="Save"]`, TimeoutMS: 3000})
	m := regexp.MustCompile(`^waited [0-9.]+ s for role=button\[name="Save"\] to be visible \(it is ([a-z][a-z0-9]e[0-9]+)\)$`).FindStringSubmatch(out)
	if err != nil || m == nil {
		t.Fatalf("visible: %v %q", err, out)
	}
	if _, err := f.act(Step{Action: "click", Ref: m[1]}); err != nil || strings.Join(p.clicked(), ",") != "save" {
		t.Errorf("its ref: %v %v", err, p.clicked())
	}
	out, err = f.waitIn(WaitIn{Target: `text="Cancel"`})
	if err != nil || !strings.HasPrefix(out, `already true when wait started: text="Cancel" is visible (it is `) {
		t.Errorf("already: %v %q", err, out)
	}
	if acts := f.m.Activity(f.sess.ID); acts[0].What != "Waited for a text target to be visible: already true" {
		t.Errorf("activity: %v", acts[0])
	}
	out, err = f.waitIn(WaitIn{Target: `role=button[name="Publish"]`, TimeoutMS: 50})
	if err != nil || out != `timed out after 0.05 s waiting for role=button[name="Publish"] to be visible; now: no visible element matches` {
		t.Errorf("never: %v %q", err, out)
	}
	// Hidden: none visible; one, or several, still showing is waited out.
	go func() {
		time.Sleep(50 * time.Millisecond)
		f.fb.mu.Lock()
		p.del1.box, p.del2.box = nil, nil
		f.fb.mu.Unlock()
	}()
	out, err = f.waitIn(WaitIn{Target: `role=button[name="Delete"]`, State: "hidden", TimeoutMS: 3000})
	if err != nil || !strings.HasPrefix(out, `waited `) || !strings.HasSuffix(out, ` s for role=button[name="Delete"] to be hidden`) {
		t.Errorf("hidden: %v %q", err, out)
	}
	out, err = f.waitIn(WaitIn{Target: `role=button[name="Save"]`, State: "hidden", TimeoutMS: 30})
	if err != nil || out != `timed out after 0.03 s waiting for role=button[name="Save"] to be hidden; now: 1 visible element matches` {
		t.Errorf("not hidden: %v %q", err, out)
	}
	// Several, for longer than ambiguousFor: act's error.
	f.fb.mu.Lock()
	p.del1.box, p.del2.box = box(10, 130, 200, 30), box(10, 170, 200, 30)
	f.fb.mu.Unlock()
	f.tools.ambiguousFor = 100 * time.Millisecond
	start := time.Now()
	_, err = f.waitIn(WaitIn{Target: `role=button[name="Delete"]`, TimeoutMS: 5000})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "matches 2 visible elements:") || len(candidateRefs(err.Error())) != 2 || time.Since(start) > 2*time.Second {
		t.Errorf("ambiguous: %v after %s", err, time.Since(start))
	}
	// At the deadline, too, if that comes first.
	f.tools.ambiguousFor = time.Minute
	_, err = f.waitIn(WaitIn{Target: `role=button[name="Delete"]`, TimeoutMS: 60})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "matches 2 visible elements:") {
		t.Errorf("ambiguous at the deadline: %v", err)
	}
	// With nth, several are fine.
	if out, err := f.waitIn(WaitIn{Target: `role=button[name="Delete"] >> nth=1`}); err != nil || !strings.HasPrefix(out, "already true") {
		t.Errorf("nth: %v %q", err, out)
	}
	for _, in := range []WaitIn{{Target: `#x`}, {Target: `text=a`, Ref: "a1e2"}, {Target: "text=a", Text: "a"}} {
		if _, err := f.waitIn(in); !isStatus(err, 400) {
			t.Errorf("%+v: %v", in, err)
		}
	}
	checkSent(t, f.fb)
}

// TestTargetsKeepTheRefCap: refs an error hands out are real refs of the
// session's map, which still holds at most maxRefs.
func TestTargetsKeepTheRefCap(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	for _, name := range []string{"One", "Two"} {
		p.doc.kids = append(p.doc.kids, el("button", "button", name, box(400, 10, 50, 20)))
	}
	f.fb.addTab("https://example.com/", p.doc, true)
	f.mustSnapshot(nil) // makes the session's state
	f.tools.mu.Lock()
	st := f.tools.pages[f.sess.ID]
	f.tools.mu.Unlock()
	tab := f.fb.tabs[0]
	// The map is full: of this document's refs, as a long session's would be.
	for i := range maxRefs {
		st.refs.ref(refKey{target: tab.id, frame: "F-" + tab.id, loader: tab.loader, backend: int64(100000 + i)})
	}
	for range 3 {
		_, err := f.act(Step{Action: "click", Target: `role=button`})
		if !isStatus(err, 409) {
			t.Fatal(err)
		}
		refs := candidateRefs(err.Error())
		if len(refs) != 5 || len(st.refs.byRef) > maxRefs {
			t.Fatalf("%d refs handed out, %d live", len(refs), len(st.refs.byRef))
		}
		for _, r := range refs {
			if _, err := st.refs.lookup(r); err != nil {
				t.Errorf("handed out %s, which isn't live: %v", r, err)
			}
		}
		if _, err := f.act(Step{Action: "hover", Ref: refs[0]}); err != nil {
			t.Errorf("a handed-out ref: %v", err)
		}
	}
}

// TestTargetOnAPageTooLarge: a target reads the whole AX tree, so it is
// refused on a page past the size cap, like snapshot.
func TestTargetOnAPageTooLarge(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	f.fb.bigDOM = maxDOMNodes + 1
	_, err := f.act(Step{Action: "click", Target: `role=button[name="Save"]`})
	if !isStatus(err, 422) || !strings.Contains(err.Error(), "page too large to resolve role= or text= targets") {
		t.Errorf("too large: %v", err)
	}
	if _, err := f.waitIn(WaitIn{Target: `text=Save`}); !isStatus(err, 422) {
		t.Errorf("wait, too large: %v", err)
	}
}

// TestTargetsTakeAReadSlot: resolving a target is a whole-page read, so it
// waits for a slot like snapshot does.
func TestTargetsTakeAReadSlot(t *testing.T) {
	f := newFixture(t)
	p := newTargetPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	f.tools.readWait = 50 * time.Millisecond
	for range maxReads {
		f.tools.reads <- struct{}{}
	}
	_, err := f.act(Step{Action: "click", Target: `role=button[name="Save"]`})
	for range maxReads {
		<-f.tools.reads
	}
	if !isStatus(err, 503) || len(p.clicked()) != 0 {
		t.Errorf("no read slot: %v %v", err, p.clicked())
	}
}
