package pagetools

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// actPage is a page with things to act on, and what each does.
type actPage struct {
	doc                                       *fnode
	email, pw, remember, signin, buy, overlay *fnode
	birthday, upload, country, first, second  *fnode
	clicks                                    []string
	mu                                        sync.Mutex
}

func newActPage(fb *fakeBrowser) *actPage {
	p := &actPage{}
	field := func(name string, y float64, attrs ...string) *fnode {
		n := el("input", "textbox", name, box(10, y, 200, 20))
		n.value = new("")
		for i := 0; i+1 < len(attrs); i += 2 {
			n.attr(attrs[i], attrs[i+1])
		}
		fb.behave[n] = &behaviour{field: "text"}
		return n
	}
	p.email = field("Email", 100, "name", "email")
	p.pw = field("Password", 130, "type", "password")
	p.remember = el("input", "checkbox", "Remember me", box(10, 160, 20, 20))
	fb.behave[p.remember] = &behaviour{field: "checkbox"}
	button := func(name string, y float64) *fnode {
		n := el("button", "button", name, box(10, y, 100, 30))
		fb.behave[n] = &behaviour{onClick: func() { p.mu.Lock(); p.clicks = append(p.clicks, name); p.mu.Unlock() }}
		return n
	}
	p.signin = button("Sign in", 200)
	p.buy = button("Buy now", 240)
	p.overlay = el("div", "region", "Promotion overlay", box(0, 0, 1280, 800))
	fb.behave[p.buy].cover = p.overlay
	p.birthday = field("Birthday", 300)
	fb.behave[p.birthday].field = "date"
	p.upload = field("Upload", 330)
	fb.behave[p.upload].field = "file"
	p.country = el("select", "combobox", "Country", box(10, 360, 100, 20))
	fb.behave[p.country] = &behaviour{options: []string{"France", "Germany"}}
	p.first = field("First", 400)
	p.second = field("Second", 430)
	fb.behave[p.first].stealsTo = p.second
	p.doc = el("#document", "RootWebArea", "", box(0, 0, 1280, 800), p.email, p.pw, p.remember, p.signin, p.buy, p.birthday, p.upload, p.country, p.first, p.second, p.overlay)
	return p
}

func (p *actPage) clicked() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.clicks...)
}

// refs takes a snapshot and returns the ref of each control by name.
func (f *fixture) refs() map[string]string {
	f.t.Helper()
	out := f.mustSnapshot(new(MaxMaxChars))
	refs := map[string]string{}
	for _, m := range regexp.MustCompile(`(?m)^([a-z][a-z0-9]e[0-9]+) \S+ "([^"]*)"`).FindAllStringSubmatch(out, -1) {
		refs[m[2]] = m[1]
	}
	return refs
}

func (f *fixture) act(steps ...Step) (string, error) {
	out, err := f.tools.Act(context.Background(), f.session(), f.who, ActIn{Steps: steps})
	f.closedAfter()
	return out, err
}

func text(s string) *string { return &s }

func TestActLogsIn(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/login", p.doc, true)
	r := f.refs()
	out, err := f.act(
		Step{Action: "fill", Ref: r["Email"], Text: text("ana@example.com")},
		Step{Action: "fill", Ref: r["Password"], Text: text("hunter2-SECRET")},
		Step{Action: "check", Ref: r["Remember me"]},
		Step{Action: "click", Ref: r["Sign in"]},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`filled ` + r["Email"] + ` textbox "Email" (typed 15 characters)`,
		`filled ` + r["Password"] + ` textbox "Password" (typed 14 characters)`,
		`checked ` + r["Remember me"] + ` checkbox "Remember me"`,
		`clicked ` + r["Sign in"] + ` button "Sign in"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if *p.email.value != "ana@example.com" || *p.pw.value != "hunter2-SECRET" || !f.fb.behave[p.remember].checked {
		t.Errorf("the page got %q, %q, %t", *p.email.value, *p.pw.value, f.fb.behave[p.remember].checked)
	}
	if got := p.clicked(); len(got) != 1 || got[0] != "Sign in" {
		t.Errorf("clicks %v", got)
	}
	// Typed text is never echoed, nor logged.
	for _, a := range f.m.Activity(f.sess.ID) {
		if strings.Contains(a.What, "SECRET") || strings.Contains(a.What, "ana@") {
			t.Errorf("activity: %q", a.What)
		}
	}
	if strings.Contains(out, "SECRET") || strings.Contains(out, "ana@") {
		t.Errorf("typed text echoed:\n%s", out)
	}
	ev := strings.Join(f.fb.input(f.fb.tabs[0]).events, "|")
	if !strings.Contains(ev, "mouseMoved Sign in|mousePressed Sign in|mouseReleased Sign in") {
		t.Errorf("mouse events: %s", ev)
	}
	if !strings.Contains(ev, "rawKeyDown Control|rawKeyDown a|keyUp a|keyUp Control|insertText") {
		t.Errorf("fill's key events: %s", ev)
	}
	checkSent(t, f.fb)
}

func TestActStopsAtTheFirstFailure(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	_, err := f.act(Step{Action: "click", Ref: r["Sign in"]}, Step{Action: "click", Ref: r["Buy now"]}, Step{Action: "click", Ref: r["Sign in"]})
	if !isStatus(err, http.StatusConflict) {
		t.Fatalf("got %v", err)
	}
	msg := err.Error()
	for _, want := range []string{`clicked ` + r["Sign in"] + ` button "Sign in"`, "Step 2 (click) failed: " + r["Buy now"] + " is covered by <untrusted-content-", `region "Promotion overlay"`, "The step after it didn't run."} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q in\n%s", want, msg)
		}
	}
	if got := p.clicked(); len(got) != 1 {
		t.Errorf("clicks %v: the covered button, or the step after, ran", got)
	}
	// 21 steps, or a bad one, are refused before anything runs.
	f.fb.Reset()
	steps := make([]Step, MaxSteps+1)
	for i := range steps {
		steps[i] = Step{Action: "click", Ref: r["Sign in"]}
	}
	for _, bad := range [][]Step{steps, nil, {{Action: "click", Ref: r["Sign in"]}, {Action: "teleport"}}, {{Action: "fill", Ref: r["Email"]}}, {{Action: "press", Keys: "ctrl+nonsense"}}, {{Action: "click"}}} {
		if _, err := f.act(bad...); !isStatus(err, 400) {
			t.Errorf("%d steps: %v", len(bad), err)
		}
	}
	if n := len(f.fb.Received()); n != 0 {
		t.Errorf("refused calls reached the browser (%d methods)", n)
	}
}

func TestActChecksTheFocus(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	_, err := f.act(Step{Action: "type", Ref: r["First"], Text: text("abc")})
	if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "the focus left "+r["First"]+" after 1 character; it went to <untrusted-content-") || !strings.Contains(err.Error(), `textbox "Second"`) {
		t.Fatalf("got %v", err)
	}
	if *p.second.value != "" {
		t.Errorf("typing went on in the field that took the focus: %q", *p.second.value)
	}
	f.fb.behave[p.email].unfocused = true
	if _, err := f.act(Step{Action: "fill", Ref: r["Email"], Text: text("x")}); !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "didn't take the focus") {
		t.Errorf("unfocusable: %v", err)
	}
	if *p.email.value != "" {
		t.Error("typed into a field without the focus")
	}
}

func TestActRefusesUnsupportedInputs(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	for name, want := range map[string]string{"Birthday": "a date input", "Upload": "files"} {
		if _, err := f.act(Step{Action: "fill", Ref: r[name], Text: text("x")}); !isStatus(err, 422) || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := f.act(Step{Action: "fill", Ref: r["Country"], Text: text("x")}); !isStatus(err, 422) {
		t.Errorf("fill a select: %v", err)
	}
	out, err := f.act(Step{Action: "select", Ref: r["Country"], Option: "Germany"})
	if err != nil || !strings.Contains(out, `selected "Germany" in `+r["Country"]+` combobox "Country"`) || f.fb.behave[p.country].selected != "Germany" {
		t.Errorf("select: %v\n%s", err, out)
	}
	if _, err := f.act(Step{Action: "select", Ref: r["Country"], Option: "Mars"}); !isStatus(err, 422) || !strings.Contains(err.Error(), "<untrusted-content-") || !strings.Contains(err.Error(), "France") {
		t.Errorf("no such option: %v", err)
	}
}

func TestActEchoesWhatItHits(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	// An SPA reuses the node for other content.
	f.fb.mu.Lock()
	p.signin.name = "Delete account"
	f.fb.mu.Unlock()
	out, err := f.act(Step{Action: "click", Ref: r["Sign in"]})
	if err != nil || !strings.Contains(out, `clicked `+r["Sign in"]+` button "Delete account"`) {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.HasPrefix(out, "<untrusted-content-") {
		t.Error("the echo isn't fenced")
	}
	// A removed node's ref is stale, and so is a detached one's.
	f.fb.mu.Lock()
	p.doc.kids = p.doc.kids[1:] // email
	f.fb.removed[p.pw] = true
	f.fb.mu.Unlock()
	for _, name := range []string{"Email", "Password"} {
		if _, err := f.act(Step{Action: "click", Ref: r[name]}); !isStatus(err, 409) || !strings.Contains(err.Error(), "stale ref "+r[name]+": take a new snapshot") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestAStepThatNavigatesMakesLaterStepsStale(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.fb.behave[p.signin].onClick = func() { f.fb.go_(tb, "https://example.com/next") }
	_, err := f.act(Step{Action: "click", Ref: r["Sign in"]}, Step{Action: "fill", Ref: r["Email"], Text: text("x")})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "Step 2 (fill) failed: stale ref") {
		t.Fatalf("got %v", err)
	}
}

func TestARefFromAnotherTabIsRefused(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.fb.addTab("https://accounts.example/", loginPage(), true)
	_, err := f.act(Step{Action: "click", Ref: r["Sign in"]})
	if !isStatus(err, 409) || !strings.Contains(err.Error(), "is in another tab (t1), not the active one") {
		t.Fatalf("got %v", err)
	}
	if len(p.clicked()) != 0 {
		t.Error("clicked in a tab that isn't active")
	}
}

func TestCancellingMidActLetsGo(t *testing.T) {
	for _, tc := range []struct {
		step  func(r map[string]string) Step
		press string
		after string
	}{
		{func(r map[string]string) Step { return Step{Action: "click", Ref: r["Sign in"]} }, "mousePressed", "mouseReleased"},
		{func(r map[string]string) Step { return Step{Action: "type", Ref: r["Email"], Text: text("abcdef")} }, "rawKeyDown", "keyUp"},
	} {
		t.Run(tc.press, func(t *testing.T) {
			f := newFixture(t)
			p := newActPage(f.fb)
			tb := f.fb.addTab("https://example.com/", p.doc, true)
			r := f.refs()
			var once sync.Once
			f.fb.onInput = func(typ string) {
				if typ == tc.press {
					once.Do(func() {
						// A handoff opens while the button or key is down.
						go func() { _, _ = f.m.SetNeedsHelp(f.sess.ID, true) }()
						time.Sleep(100 * time.Millisecond)
					})
				}
			}
			_, err := f.act(tc.step(r))
			if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "handoff") {
				t.Fatalf("got %v", err)
			}
			f.fb.mu.Lock()
			ev := f.fb.input(tb).events
			f.fb.mu.Unlock()
			pressed := -1
			for i, e := range ev {
				if strings.HasPrefix(e, tc.press) && pressed < 0 {
					pressed = i
				}
			}
			released := false
			for _, e := range ev[pressed+1:] {
				released = released || strings.HasPrefix(e, tc.after)
			}
			if pressed < 0 || !released {
				t.Errorf("events %v: pressed but never let go", ev)
			}
			if strings.Count(strings.Join(ev, "|"), tc.press) > 1 {
				t.Errorf("events %v: went on after the cancel", ev)
			}
		})
	}
}

func TestConcurrentActsDontInterleave(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	f.tools.settle = 20 * time.Millisecond
	var wg sync.WaitGroup
	for _, name := range []string{"Sign in", "Remember me"} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			steps := []Step{{Action: "click", Ref: r[name]}, {Action: "click", Ref: r[name]}, {Action: "click", Ref: r[name]}}
			if _, err := f.tools.Act(context.Background(), f.session(), f.who, ActIn{Steps: steps}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	f.fb.mu.Lock()
	var seq []string
	for _, e := range f.fb.input(tb).events {
		if strings.HasPrefix(e, "mousePressed") {
			seq = append(seq, strings.TrimPrefix(e, "mousePressed "))
		}
	}
	f.fb.mu.Unlock()
	got := strings.Join(seq, ",")
	if got != "Sign in,Sign in,Sign in,Remember me,Remember me,Remember me" && got != "Remember me,Remember me,Remember me,Sign in,Sign in,Sign in" {
		t.Errorf("interleaved: %s", got)
	}
}

func TestPressHoverAndSnapshotAfter(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	out, err := f.tools.Act(context.Background(), f.session(), f.who, ActIn{Snapshot: true, Steps: []Step{
		{Action: "hover", Ref: r["Sign in"]},
		{Action: "press", Keys: "Tab shift+Tab Enter"},
		{Action: "press", Keys: "x"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `hovered over `+r["Sign in"]+` button "Sign in"`) || !strings.Contains(out, "pressed Tab shift+Tab Enter") || strings.Count(out, "<untrusted-content-") != 2 || !strings.Contains(out, "URL: https://example.com/") {
		t.Errorf("result:\n%s", out)
	}
	ev := strings.Join(f.fb.input(tb).events, "|")
	if strings.Contains(ev, "mousePressed") || !strings.Contains(ev, "rawKeyDown Shift|rawKeyDown Tab|keyUp Tab|keyUp Shift") || !strings.Contains(ev, "rawKeyDown Enter|char Enter|keyUp Enter") {
		t.Errorf("events: %s", ev)
	}
	acts := fmt.Sprint(f.m.Activity(f.sess.ID))
	if !strings.Contains(acts, "Pressed Tab, shift+Tab, Enter") || !strings.Contains(acts, "Pressed 1 key") || !strings.Contains(acts, "Hovered over "+r["Sign in"]) {
		t.Errorf("activity: %s", acts)
	}
}

func TestWaitFor(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	tb := f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	wait := func(in WaitIn) (string, error) {
		out, err := f.tools.WaitFor(context.Background(), f.session(), f.who, in)
		f.closedAfter()
		return out, err
	}
	for _, bad := range []WaitIn{{}, {Text: "a", URLChange: true}, {Text: "a", TimeoutMS: 30001}, {Text: "a", TimeoutMS: -1}, {Ref: r["Email"], State: "gone"}, {Text: "a", State: "hidden"}} {
		if _, err := wait(bad); !isStatus(err, 400) {
			t.Errorf("%+v: %v", bad, err)
		}
	}
	// Text that appears later.
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.fb.mu.Lock()
		p.doc.kids = append(p.doc.kids, txt("Payment confirmed", box(0, 500, 100, 20)))
		f.fb.mu.Unlock()
	}()
	if out, err := wait(WaitIn{Text: "payment CONFIRMED"}); err != nil || out != "ok" {
		t.Errorf("text: %v %q", err, out)
	}
	if out, err := wait(WaitIn{Text: "never", TimeoutMS: 150}); err != nil || out != "timed out after 150ms" {
		t.Errorf("timeout: %v %q", err, out)
	}
	// An element hidden later; then one removed.
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.fb.mu.Lock()
		p.signin.box = nil
		f.fb.mu.Unlock()
	}()
	if out, err := wait(WaitIn{Ref: r["Sign in"], State: "hidden"}); err != nil || out != "ok" {
		t.Errorf("hidden: %v %q", err, out)
	}
	if out, err := wait(WaitIn{Ref: r["Email"]}); err != nil || out != "ok" {
		t.Errorf("visible: %v %q", err, out)
	}
	// The URL changes: a new document, so the isolated world is made again.
	worlds := 0
	go func() {
		time.Sleep(100 * time.Millisecond)
		f.fb.mu.Lock()
		f.fb.go_(tb, "https://example.com/done")
		f.fb.mu.Unlock()
	}()
	if out, err := wait(WaitIn{URLChange: true}); err != nil || out != "ok" {
		t.Errorf("url_change: %v %q", err, out)
	}
	for _, m := range f.fb.Received() {
		if m.Method == "Page.createIsolatedWorld" {
			worlds++
		}
	}
	if worlds < 2 {
		t.Errorf("%d isolated worlds made", worlds)
	}
	// The removed element's ref counts as hidden.
	if out, err := wait(WaitIn{Ref: r["Email"], State: "hidden"}); err != nil || out != "ok" {
		t.Errorf("hidden after navigation: %v %q", err, out)
	}
	checkSent(t, f.fb)
}

func TestAHandoffEndsAWaitAtOnce(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	done := make(chan error, 1)
	go func() {
		_, err := f.tools.WaitFor(context.Background(), f.session(), f.who, WaitIn{Text: "never", TimeoutMS: 20000})
		done <- err
	}()
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	if _, err := f.m.SetNeedsHelp(f.sess.ID, true); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !isStatus(err, http.StatusConflict) || !strings.Contains(err.Error(), "handoff") {
			t.Errorf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait went on through the handoff")
	}
	if d := time.Since(start); d > time.Second {
		t.Errorf("the handoff waited %s for the call", d)
	}
	f.closedAfter()
}
