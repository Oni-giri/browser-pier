package browsertest

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// PageSite serves the pages PROJ-21's page tools are tested on. Pages
// report what a person would have done (a click, a submitted form) by
// fetching /event, which Events lists.
type PageSite struct {
	URL string

	mu     sync.Mutex
	events []string
	logins []string // user:password of each login posted
}

// Events lists what the pages reported, oldest first.
func (s *PageSite) Events() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.events...)
}

// HasEvent reports whether a page reported e.
func (s *PageSite) HasEvent(e string) bool {
	for _, got := range s.Events() {
		if got == e {
			return true
		}
	}
	return false
}

// Logins lists the logins posted to /login, as "user:password".
func (s *PageSite) Logins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.logins...)
}

// report is a script that tells the site about an event.
const report = `<script>function report(e) { fetch('/event?e=' + encodeURIComponent(e)); }</script>`

var pageSitePages = map[string]string{ //nolint:gosec // test pages, not credentials
	// A login form with no <form>-level tricks: email, password, a button.
	"/login": `<!doctype html><title>Sign in</title>` + report + `
<main><h1>Sign in</h1>
<form method="post" action="/login">
<label for="email">Email</label> <input id="email" name="email" autocomplete="username">
<label for="password">Password</label> <input id="password" name="password" type="password" autocomplete="current-password">
<label><input type="checkbox" name="remember"> Remember me</label>
<button>Sign in</button>
</form></main>`,
	// A long form: its last fields are far below the fold.
	"/long-form": `<!doctype html><title>Long form</title><main><h1>Application</h1><form>` + longForm() + `<button type="button" onclick="this.textContent='Sent'">Send application</button></form></main>`,
	// A nav of 200 links and a cookie banner before a small form.
	"/nav": `<!doctype html><title>Big menu</title>
<div id="cookies" style="position:fixed;bottom:0;left:0;right:0;background:#eee;padding:8px">We use cookies. <button>Accept all</button> <button>Reject</button> <a href="#">Settings</a></div>
<nav>` + navLinks() + `</nav>
<main><h1>Newsletter</h1><label for="nl">Your email</label> <input id="nl" name="newsletter-email"><button>Subscribe</button></main>`,
	// An open modal over the page.
	"/modal": `<!doctype html><title>Modal</title>` + report + `
<main><h1>Article</h1><p>Some article text behind the dialog.</p><button>Behind the dialog</button></main>
<dialog id="d"><h2>Before you go</h2><p>Confirm your choice.</p><button onclick="report('confirmed')">Confirm</button> <button onclick="this.closest('dialog').close()">Cancel</button></dialog>
<script>document.getElementById('d').showModal()</script>`,
	// A transparent overlay covering a button.
	"/overlay": `<!doctype html><title>Overlay</title>` + report + `
<button id="buy" onclick="report('buy clicked')" style="position:absolute;left:50px;top:50px;width:200px;height:60px">Buy now</button>
<div aria-label="Promotion overlay" role="region" style="position:fixed;inset:0;background:transparent" onclick="report('overlay clicked')"></div>`,
	// A styled checkbox: the input sits hidden under its label.
	"/checkbox": `<!doctype html><title>Checkbox</title>` + report + `
<style>.box{position:relative;display:inline-block;padding:10px 10px 10px 40px;border:1px solid #888}
.box input{position:absolute;left:12px;top:12px;opacity:0;margin:0}
.box::before{content:'';position:absolute;left:10px;top:10px;width:20px;height:20px;border:2px solid #333}</style>
<label class="box"><input type="checkbox" id="terms" onchange="report('terms ' + this.checked)">I accept the terms</label>
<p><label><input type="checkbox" style="display:none" onchange="report('news ' + this.checked)"> Send me news</label></p>`,
	// Text a person can't see: some is excluded, some kept (fenced).
	"/hidden": `<!doctype html><title>Hidden text</title>
<p>Visible text.</p>
<p style="display:none">DISPLAY-NONE-TEXT</p>
<p style="visibility:hidden">VISIBILITY-HIDDEN-TEXT</p>
<p aria-hidden="true">ARIA-HIDDEN-TEXT</p>
<p>Before <span style="font-size:0">ZERO-SIZE-TEXT</span> after.</p>
<button style="width:0;height:0;padding:0;border:0;overflow:hidden">ZERO-SIZE-BUTTON</button>
<div style="position:absolute;width:1px;height:1px;overflow:hidden;clip-path:inset(50%)">CLIPPED-TEXT</div>
<p style="opacity:0">OPACITY-ZERO-TEXT</p>
<p style="position:absolute;left:-9999px">OFF-SCREEN-TEXT</p>
<span id="lbl" style="display:none">LABELLEDBY-HIDDEN-NAME</span>
<button aria-labelledby="lbl">x</button>
<button aria-label="ARIA-LABEL-NAME">y</button>`,
	// A password field the page filled in itself.
	"/password": `<!doctype html><title>Saved password</title>
<label for="u">User</label> <input id="u" value="pier">
<label for="p">Password</label> <input id="p" type="password" value="PAGE-FILLED-SECRET">
<label for="otp">One-time code</label> <input id="otp" autocomplete="one-time-code" value="424242">`,
	// A list that re-renders: Next renames the first item in place (the
	// node is reused) and removes the second.
	"/spa": `<!doctype html><title>Inbox</title>` + report + `
<ul id="list"><li><button onclick="report('opened ' + this.textContent)">Message one</button></li><li><button>Message two</button></li></ul>
<button id="next" onclick="const l=document.getElementById('list');l.children[0].firstChild.textContent='Message three';l.children[1].remove()">Next page</button>`,
	// A link to a new tab and a login popup.
	"/popups": `<!doctype html><title>Popups</title>` + report + `
<a href="/popup-login" target="_blank">Open in a new tab</a>
<button onclick="window.open('/popup-login', 'login', 'popup,width=500,height=500')">Sign in with a popup</button>`,
	"/popup-login": `<!doctype html><title>Popup sign-in</title>` + report + `
<label for="pe">Email</label> <input id="pe"><button onclick="report('popup signed in'); setTimeout(() => window.close(), 100)">Continue</button>`,
	// An alert after a click.
	"/alert": `<!doctype html><title>Alert</title><button onclick="alert('Hello from the page')">Show an alert</button>`,
	// Text that appears after 2 s, and an element hidden after 2 s.
	"/later": `<!doctype html><title>Later</title>
<p id="status">Working…</p><div id="spinner">Loading spinner</div>
<script>setTimeout(() => { document.getElementById('status').textContent = 'Payment confirmed'; document.getElementById('spinner').style.display = 'none'; }, 2000)</script>`,
	// A page with an iframe.
	// A news page under a consent banner: an iframe (from ?frame=, another
	// site in tests) fixed on top of the page's own consent button
	// (PROJ-110). Around it, what no iframe covers: a button clear of the
	// banner, one in the page's own layer on top of an iframe behind it (a
	// negative z-index, which Chromium numbers after that layer), one under
	// an ad iframe that clicks go through.
	"/consent": `<!doctype html><title>News</title>` + report + `
<style>body{margin:0} iframe{border:0}</style>
<button style="position:absolute;left:10px;top:10px;width:100px;height:30px">Menu</button>
<div style="margin-top:60px;height:150px">
<iframe id="video" title="Video" style="position:absolute;left:0;top:60px;width:600px;height:150px;z-index:-1"></iframe>
<button style="margin:50px">Watch</button></div>
<button style="position:absolute;left:300px;top:310px">Behind the ad</button>
<div style="position:fixed;left:0;top:300px;width:100%;height:60px;pointer-events:none"><iframe title="Ad" style="width:100%;height:60px"></iframe></div>
<button onclick="report('page agree')" style="position:absolute;left:500px;top:450px;width:150px;height:40px">Yes, I agree</button>
<iframe id="consent" title="SP Consent Message" style="position:fixed;left:0;top:400px;width:100%;height:400px;z-index:2147483647;background:#fff"></iframe>
<script>document.getElementById('consent').src = new URLSearchParams(location.search).get('frame') || '/consent-frame';
document.getElementById('video').src = '/consent-frame';</script>`,
	"/consent-frame": `<!doctype html><title>Consent</title><p>We value your privacy.</p><button>Accept</button>`,
	"/frame":         `<!doctype html><title>Frame</title><p>Outside the frame.</p><iframe src="/login" width="400" height="300"></iframe>`,
	// A field that takes focus away from itself as you type.
	"/thief": `<!doctype html><title>Thief</title><input id="a" aria-label="First" oninput="document.getElementById('b').focus()"><input id="b" aria-label="Second">`,
	// A select and a date input.
	"/select": `<!doctype html><title>Select</title>` + report + `
<label for="c">Country</label> <select id="c" onchange="report('country ' + this.value)"><option value="fr">France</option><option value="de">Germany</option><option value="lv">Latvia</option></select>
<label for="d">Date</label> <input id="d" type="date">
<button onmouseover="report('hovered')">Hover me</button>
<label>Upload <input type="file"></label>`,
	"/signed-in": `<!doctype html><title>Account</title><h1>Signed in</h1>`,
	// An article among a site's furniture, for the content endpoints
	// (PROJ-20): a nav, a cookie banner, a sidebar, hidden and transparent
	// text, a page-filled password, a closed shadow root and an iframe.
	"/article": `<!doctype html><title>Library opens</title>
<div id="cookies" style="position:fixed;bottom:0;left:0;right:0;background:#eee;padding:8px" class="cookie-banner">We use cookies. <button>Accept all</button></div>
<header><a href="/">Example News</a></header>
<nav><a href="/world">World</a> <a href="/sport">Sport</a> <a href="/weather">Weather</a></nav>
<main><article>
<h1>Library opens next spring</h1>
<p>The committee met on Tuesday to discuss the new library, which opens next spring. Members agreed on the budget, the opening hours and the staffing plan, after a long debate about parking. Read <a href="/plan.pdf">the full plan</a>.</p>
<p>The building has three floors, a reading garden and a children's wing. Volunteers will run a book swap on Saturdays, and the café will stay open late on Thursdays, the committee said.</p>
<h2>Opening hours</h2>
<table><tr><th>Day</th><th>Hours</th></tr><tr><td>Monday</td><td>9–18</td></tr><tr><td>Saturday</td><td>10–16</td></tr></table>
<ul><li>Free cards for residents</li><li>Late returns forgiven in May</li></ul>
<pre><code class="language-sh">curl https://library.example/api</code></pre>
<p style="display:none">DISPLAY-NONE-TEXT</p>
<p style="visibility:hidden">VISIBILITY-HIDDEN-TEXT</p>
<p style="opacity:0">OPACITY-ZERO-TEXT</p>
<div id="host"></div>
<label for="pw">Password</label> <input id="pw" type="password" value="PAGE-FILLED-SECRET">
</article>
<aside class="sidebar">Most read: <a href="/x">Something else</a></aside></main>
<iframe src="/login" width="300" height="200"></iframe>
<footer>© Example News</footer>
<script>document.getElementById('host').attachShadow({mode: 'closed'}).innerHTML = '<p>Text in a closed shadow root.</p>';</script>`,
	// Fields and hidden things, for what content leaves out (PROJ-20 review).
	"/probe-fields": `<!doctype html><title>Fields</title>
<style>#g::before{content:"GENERATED-TEXT"}</style>
<p>VISIBLE-PARA</p>
<input id=t type=text><input id=otp autocomplete=one-time-code><textarea id=ta>TA-DEFAULT</textarea>
<select><option>OPT-ONE<option selected>OPT-TWO</select>
<div contenteditable id=ce>CE-TEXT</div>
<input type=submit value=SUBMIT-VALUE><input placeholder=PLACEHOLDER-TEXT>
<output>OUTPUT-TEXT</output>
<noscript>NOSCRIPT-TEXT</noscript><template>TEMPLATE-TEXT</template>
<details><summary>SUM</summary>DETAILS-CLOSED</details>
<p aria-hidden=true>ARIA-HIDDEN</p>
<p style="font-size:0">FONT-ZERO</p>
<p style="position:absolute;left:-9999px">OFFSCREEN</p>
<div style="display:contents"><p>CONTENTS-TEXT</p></div>
<p style="height:0;overflow:hidden">CLIPPED-ZERO</p>
<p id=g></p>
<img alt=HIDDEN-ALT style=display:none src=x><img alt=BROKEN-ALT src=nope.png width=40 height=40>
<x-f id=xf></x-f>
<a href="javascript:alert(1)">JS-LINK</a> <a href="/rel?a=(1)">REL-LINK</a>
<iframe src="http://127.0.0.1:9/%zz&#10;# FAKE-HEADING&#10;" width=50 height=50></iframe>
<iframe src="data:text/html,hi]&#10;# FAKE-TWO" width=50 height=50></iframe>
<p>code sample: &lt;!-- a comment</p><p>AFTER-COMMENT</p>
<script>t.value='JS-TEXT-VALUE'; otp.value='OTP-123456'; ta.value='TA-TYPED';
xf.attachShadow({mode:'closed'}).innerHTML='<input value=SHADOW-INPUT-VALUE><p>SHADOW-P</p>';</script>`,
	"/probe.xhtml": `<?xml version="1.0"?><html xmlns="http://www.w3.org/1999/xhtml" xmlns:h="http://www.w3.org/1999/xhtml"><head><title>X</title></head><body>
<p>XHTML-PARA</p><h:input type="password" value="XHTML-PREFIXED-SECRET"/><h:textarea>XHTML-TEXTAREA</h:textarea><h:script>var XHTMLSCRIPT=1;</h:script><h:style>.XHTMLSTYLE{}</h:style></body></html>`,
	// A hidden chain of elements, d deep, c times (PROJ-20 review).
	"/probe-deep": `<!doctype html><title>Deep</title><p>DEEP-PAGE</p><div id="d" style="display:none"></div><script>
const q = new URLSearchParams(location.search); const depth = +q.get('d'), chains = +q.get('c'), tag = q.get('t') || 'em';
const d = document.getElementById('d');
for (let c = 0; c < chains; c++) { let cur = d; for (let i = 0; i < depth; i++) { const e = document.createElement(tag); cur.appendChild(e); cur = e; } }
document.title = 'Deep ready';</script>`,
	// n elements sharing one long class, text or alt (PROJ-20 review).
	"/probe-shared": `<!doctype html><title>Shared</title><p>SHARED-PAGE</p><div id="d"></div><script>
const q = new URLSearchParams(location.search); const n = +q.get('n'), kb = +q.get('kb'), kind = q.get('k');
const d = document.getElementById('d'); const f = document.createDocumentFragment();
const cls = 'abcdefg hijkl '.repeat(kb * 73); const ws = ' '.repeat(kb * 1024);
for (let i = 0; i < n; i++) {
  if (kind === 'class') { const e = document.createElement('div'); e.className = cls; e.textContent = 'x'; f.appendChild(e); }
  else if (kind === 'text') { const e = document.createElement('span'); e.appendChild(document.createTextNode(ws)); f.appendChild(e); }
  else if (kind === 'alt') { const e = document.createElement('img'); e.alt = ws; e.width = 1; e.height = 1; e.src = 'data:image/gif;base64,R0lGODlhAQABAAAAACH5BAEKAAEALAAAAAABAAEAAAICTAEAOw=='; f.appendChild(e); }
}
d.appendChild(f); document.title = 'Shared ready';</script>`,
	// display:contents in its forms, hidden content and a deep tree (PROJ-20 review).
	"/probe-contents": `<!doctype html><title>Contents</title>
<p>line one<br>line two</p>
<input id=i1 style="display:contents"><textarea id=t1 style="display:contents">TA-CONTENTS-DEFAULT</textarea>
<select style="display:contents"><option selected>SEL-CONTENTS</option></select>
<select size=3 multiple><option style="display:contents">LISTBOX-OPT</option></select>
<button style="display:contents">BUTTON-CONTENTS</button>
<div style="display:contents"><input id=i2 value="ATTR-VALUE-IN-CONTENTS"></div>
<details style="display:contents"><summary>S</summary>DETAILS-CONTENTS-CLOSED</details>
<div style="display:none"><div style="display:contents"><p style="display:block">NONE-THEN-CONTENTS</p></div></div>
<div style="visibility:hidden"><p>VIS-HIDDEN <span style="visibility:visible">VIS-REVEALED</span></p></div>
<div style="content-visibility:hidden"><p>CV-HIDDEN</p></div>
<a href="/contents-link" style="display:contents">CONTENTS-LINK</a>
<ul style="display:contents"><li>LI-ONE</li><li>LI-TWO</li></ul>
<table><tr style="display:contents"><td>CELL-A</td><td>CELL-B</td></tr></table>
<article style="display:contents"><h1 style="display:contents">CONTENTS-HEADING</h1><p>Article body, long enough, with commas, to be an article of its own, and then some more text, to be sure of it, and more, and more, and more, and more again, until it passes the two hundred characters that an article needs to count.</p></article>
<nav style="display:contents"><a href="/n1">NAV-IN-CONTENTS one</a> <a href="/n2">NAV-IN-CONTENTS two</a></nav>
<div id="deep"></div>
<script>i1.value='TYPED-IN-CONTENTS-INPUT'; t1.value='TYPED-IN-CONTENTS-TA';
let cur = document.getElementById('deep'); for (let i = 0; i < 300; i++) { const e = document.createElement('div'); cur.appendChild(e); cur = e; }
cur.innerHTML = '<p>DEEP-VISIBLE <a href="/deep-link">DEEP-LINK</a><br>DEEP-SECOND <input value="DEEP-INPUT-VALUE"><img alt="DEEP-ALT" width=20 height=20 src=x></p>';</script>`,
	// Megabytes of hidden text (display:none, or a JSON script) before an article (PROJ-20 review).
	"/probe-budget": `<!doctype html><title>Budget</title><p>BEFORE-BIG</p><div id="h"></div><script>
const q = new URLSearchParams(location.search); const mb = +q.get('mb'), kind = q.get('k');
const big = 'var words = 1; '.repeat(mb * 69906);
const h = document.getElementById('h');
if (kind === 'script') { const s = document.createElement('script'); s.type = 'application/json'; s.textContent = big; h.appendChild(s); }
else if (kind === 'none') { for (let i = 0; i < mb; i++) { const d = document.createElement('div'); d.style.display = 'none'; d.textContent = 'var words = 1; '.repeat(69906); h.appendChild(d); } }
document.title = 'Budget ready';</script>
<article><h1>AFTER-BIG-HEADING</h1><p>Article body, long enough, with commas, to be an article of its own, and then some more text, to be sure of it, and more, and more, and more, and more again, until it passes the two hundred characters that an article needs to count. AFTER-BIG-END</p></article>`,
	// A page with a quarter of a million elements.
	"/huge": `<!doctype html><title>Huge</title><div id="d"></div><script>
const d = document.getElementById('d'); const f = document.createDocumentFragment();
for (let i = 0; i < 250000; i++) { const s = document.createElement('i'); f.appendChild(s); }
d.appendChild(f);</script>`,
	// Buttons inside a closed shadow root: one free, one under a cover.
	"/shadow": `<!doctype html><title>Shadow</title>` + report + `
<div id="host"></div>
<script>
const root = document.getElementById('host').attachShadow({mode: 'closed'});
root.innerHTML = '<button onclick="report(\'shadow free clicked\')">Free in shadow</button>' +
  '<span style="position:relative;display:inline-block;margin-left:20px"><button onclick="report(\'shadow covered clicked\')">Covered in shadow</button>' +
  '<span aria-label="Shadow cover" role="img" style="position:absolute;inset:0"></span></span>';
</script>`,
}

func longForm() string {
	var b strings.Builder
	for i := 1; i <= 40; i++ {
		fmt.Fprintf(&b, `<p><label for="f%d">Field %d</label> <input id="f%d" name="f%d"></p>`, i, i, i, i)
	}
	return b.String()
}

func navLinks() string {
	var b strings.Builder
	for i := 1; i <= 200; i++ {
		fmt.Fprintf(&b, `<a href="#s%d">Section %d</a> `, i, i)
	}
	return b.String()
}

// NewPageSite serves the pages on every interface; host is how the
// browser container reaches this machine.
func NewPageSite(t *testing.T, host string) *PageSite {
	t.Helper()
	s := &PageSite{}
	mux := http.NewServeMux()
	for path, body := range pageSitePages {
		mux.HandleFunc("GET "+path, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, body) })
	}
	mux.HandleFunc("POST /login", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.logins = append(s.logins, r.FormValue("email")+":"+r.FormValue("password"))
		s.mu.Unlock()
		http.Redirect(w, r, "/signed-in", http.StatusSeeOther)
	})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	mux.HandleFunc("GET /event", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.events = append(s.events, r.URL.Query().Get("e"))
		s.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})
	ln, err := net.Listen("tcp", ":0") //nolint:gosec // the browser container reaches it over the bridge
	if err != nil {
		t.Fatal(err)
	}
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if strings.HasSuffix(r.URL.Path, ".xhtml") {
			w.Header().Set("Content-Type", "application/xhtml+xml")
		}
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.URL = fmt.Sprintf("http://%s:%d", host, ln.Addr().(*net.TCPAddr).Port)
	return s
}
