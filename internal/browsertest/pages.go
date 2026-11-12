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
<label class="box"><input type="checkbox" id="terms" onchange="report('terms ' + this.checked)">I accept the terms</label>`,
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
	"/frame": `<!doctype html><title>Frame</title><p>Outside the frame.</p><iframe src="/login" width="400" height="300"></iframe>`,
	// A field that takes focus away from itself as you type.
	"/thief": `<!doctype html><title>Thief</title><input id="a" aria-label="First" oninput="document.getElementById('b').focus()"><input id="b" aria-label="Second">`,
	// A select and a date input.
	"/select": `<!doctype html><title>Select</title>` + report + `
<label for="c">Country</label> <select id="c" onchange="report('country ' + this.value)"><option value="fr">France</option><option value="de">Germany</option><option value="lv">Latvia</option></select>
<label for="d">Date</label> <input id="d" type="date">
<button onmouseover="report('hovered')">Hover me</button>`,
	"/signed-in": `<!doctype html><title>Account</title><h1>Signed in</h1>`,
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
		mux.ServeHTTP(w, r)
	})
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	s.URL = fmt.Sprintf("http://%s:%d", host, ln.Addr().(*net.TCPAddr).Port)
	return s
}
