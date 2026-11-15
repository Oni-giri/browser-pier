package browsertest

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
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

	mu      sync.Mutex
	events  []string
	logins  []string // user:password of each login posted
	uploads []Upload // each file posted to /upload
}

// Upload is a file a page's form posted to /upload (PROJ-117).
type Upload struct {
	Field, Name string
	Size        int64
	SHA256      string // hex
}

// Uploads lists the files posted to /upload, oldest first.
func (s *PageSite) Uploads() []Upload {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Upload(nil), s.uploads...)
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
	// PROJ-115 A: a 60 px sticky header, and a button the page scrolls
	// under it on load, where scrolling it just into view leaves it.
	"/sticky": `<!doctype html><title>Sticky header</title>` + report + `
<body style="margin:0">
<header aria-label="Site header" style="position:sticky;top:0;height:60px;background:#ccd;z-index:1" onclick="report('header clicked')">Site header</header>
<div style="height:440px"></div>
<button onclick="report('sticky clicked')" style="display:block;height:30px">Under the header</button>
<div style="height:2000px"></div>
<script>window.scrollTo(0, 480)</script></body>`,
	// PROJ-115 B: a full-screen backdrop over a button that starts fading
	// 200 ms after load and is gone 500 ms later, at 700 ms: still there
	// when a call made right after navigate looks (navigate returns
	// within about 100 ms of load), gone well within a 1 s wait.
	"/fading": `<!doctype html><title>Fading backdrop</title>` + report + `
<button onclick="report('fading clicked')" style="position:absolute;left:50px;top:50px;width:200px;height:60px">Behind the backdrop</button>
<div id="backdrop" role="region" aria-label="Fading backdrop" style="position:fixed;inset:0;background:rgba(0,0,0,.5);transition:opacity .5s linear" onclick="report('backdrop clicked')"></div>
<script>addEventListener('load', () => setTimeout(() => {
  const b = document.getElementById('backdrop');
  b.style.opacity = '0';
  setTimeout(() => b.remove(), 500);
}, 200))</script>`,
	// PROJ-115 C: a cookie dialog that stays, over a button.
	"/cookie-dialog": `<!doctype html><title>Cookie dialog</title>` + report + `
<button onclick="report('behind cookie clicked')" style="position:absolute;left:50px;top:50px;width:200px;height:60px">Behind the cookie dialog</button>
<div style="position:fixed;inset:0;background:rgba(0,0,0,.4)" onclick="report('cookie backdrop clicked')"></div>
<div role="dialog" aria-labelledby="cookie-title" style="position:fixed;left:40%;top:40%;background:#fff;padding:20px">
<h2 id="cookie-title">Cookie settings</h2><p>Choose which cookies we may use.</p><button>Save choices</button></div>`,
	// PROJ-115 D: a link that wraps: its first line is a 3 px sliver at
	// the end of the line, its second is long. A click reports what it
	// hit, where, and the link's line boxes then.
	"/wrapped": `<!doctype html><title>Wrapped link</title>` + report + `
<body style="margin:0"><div style="width:600px;font:16px/20px sans-serif;margin:20px 0 0 0"><span style="display:inline-block;width:597px;height:10px"></span><a id="wrapped" href="#" onclick="event.preventDefault()"><span style="display:inline-block;width:3px;height:10px"></span> Read the whole article about wrapped links here</a></div>
<script>document.addEventListener('click', e => {
  const rects = Array.from(document.getElementById('wrapped').getClientRects()).map(r => [r.left, r.top, r.width, r.height]);
  report('wrapped click ' + (e.target.closest('a') ? 'link' : e.target.localName) + ' ' + JSON.stringify([e.clientX, e.clientY, rects]));
})</script></body>`,
	// A button that opens a menu of three links (PROJ-114's changes); a
	// link reports its click and goes nowhere.
	"/menu": `<!doctype html><title>Menu</title>` + report + `
<main><h1>Shop</h1>
<button id="menu" aria-expanded="false" aria-controls="list" onclick="const open = this.getAttribute('aria-expanded') === 'true'; this.setAttribute('aria-expanded', String(!open)); document.getElementById('list').hidden = open">Menu</button>
<ul id="list" hidden>
<li><a href="#one" onclick="event.preventDefault(); report('menu One')">One</a></li>
<li><a href="#two" onclick="event.preventDefault(); report('menu Two')">Two</a></li>
<li><a href="#three" onclick="event.preventDefault(); report('menu Three')">Three</a></li>
</ul>
<p>Free delivery on every order.</p></main>`,
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
	// PROJ-125: a sign-in that sends its password in the URL (a GET
	// form), to /echo, which shows it in the title and the page.
	"/get-signin": `<!doctype html><title>Old sign-in</title>
<form method="get" action="/echo">
<label for="p">Password</label> <input id="p" name="p" type="password">
<button>Sign in</button>
</form>`,
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
	// PROJ-122's waits: text already there, text 1.3 s after the script
	// runs (about 1 s after navigate's answer, which watches 300 ms for a
	// redirect after the load), an app that
	// loads, then fetches three times in a row (400 ms each, /slow) before
	// it renders, and one whose only fetch never answers (/hang).
	"/hello": `<!doctype html><title>Hello</title><p>Hello</p>`,
	"/done-later": `<!doctype html><title>Done later</title><p id="s">Working</p>
<script>setTimeout(() => { document.getElementById('s').textContent = 'Done'; }, 1300)</script>`,
	"/spa-load": `<!doctype html><title>App</title><p id="s">Loading</p>
<script>addEventListener('load', async () => {
  for (let i = 1; i <= 3; i++) await fetch('/slow?n=' + i);
  document.getElementById('s').textContent = 'Ready';
});</script>`,
	"/longpoll": `<!doctype html><title>Long poll</title><p>Live updates</p>
<script>addEventListener('load', () => { fetch('/hang').catch(() => {}); });</script>`,
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
	// PROJ-123: two buttons named alike, a hidden third, and a paragraph
	// with a link in it, for role and text targets.
	"/two-deletes": `<!doctype html><title>Two deletes</title>` + report + `
<main><h1>Your files</h1>
<ul><li>report.pdf <button onclick="report('delete 1')">Delete</button></li>
<li>notes.txt <button onclick="report('delete 2')">Delete</button></li>
<li style="display:none">old.txt <button onclick="report('delete hidden')">Delete</button></li></ul>
<p>Deleted files stay in the bin for 30 days; <a href="#bin" onclick="report('bin')">open the bin</a> to restore one.</p>
<button onclick="setTimeout(() => { const b = document.createElement('button'); b.textContent = 'Undo'; b.onclick = () => report('undo'); document.querySelector('main').append(b); }, 1000)">Offer a way back</button>
</main>`,
	// A sign-in bounce by script (PROJ-121): a session check, a callback,
	// then the page; a page that never stops redirecting; one that sends
	// itself to an address the browser can't reach (link-local: the
	// egress filter always refuses it).
	"/hop1":     `<!doctype html><title>Checking your session</title><p>One moment.</p><script>setTimeout(() => location.replace('/hop2'), 200)</script>`,
	"/hop2":     `<!doctype html><title>Signing you in</title><p>Almost there.</p><script>setTimeout(() => location.replace('/landed'), 200)</script>`,
	"/landed":   `<!doctype html><title>Landed</title><h1>Welcome back</h1><p>LANDED-PAGE</p>`,
	"/loop":     `<!doctype html><title>Loop</title><p>Redirecting.</p><script>setTimeout(() => location.replace('/loop?n=' + (Number(new URLSearchParams(location.search).get('n')) + 1)), 100)</script>`,
	"/hop-fail": `<!doctype html><title>Checking your session</title><script>setTimeout(() => location.replace('http://169.254.169.254/sso'), 200)</script>`,
	"/sections": `<!doctype html><title>Sections</title><h1>Contents</h1><a href="#section">To the section</a><div style="height:3000px"></div><h2 id="section">The section</h2><p>SECTION-TEXT</p>`,
	// A form that posts files (PROJ-117): a plain file input, a multiple
	// one, inputs styled out of sight under their labels (transparent,
	// which the snapshot still lists; transparent and aria-hidden, and
	// display:none, which it doesn't, so only their labels have refs),
	// and a button that opens a chooser with a script, which act can't
	// answer. /upload echoes each file's name, size and SHA-256.
	"/upload": `<!doctype html><title>Upload</title>` + report + `
<style>.pick{position:relative;display:inline-block;padding:10px 20px;border:2px solid #333;border-radius:6px;cursor:pointer}
.pick input{position:absolute;inset:0;opacity:0;cursor:pointer}</style>
<main><h1>Send documents</h1>
<form method="post" action="/upload" enctype="multipart/form-data">
<p><label for="doc">Document</label> <input id="doc" name="doc" type="file" onchange="report('doc ' + this.files.length)"></p>
<p><label for="more">Attachments</label> <input id="more" name="more" type="file" multiple onchange="report('more ' + this.files.length)"></p>
<p><label class="pick">Attach a photo<input name="photo" type="file" accept="image/*" onchange="report('photo ' + this.files.length)"></label></p>
<p><label class="pick">Attach an avatar<input name="avatar" type="file" aria-hidden="true" tabindex="-1" onchange="report('avatar ' + this.files.length)"></label></p>
<p><label class="pick">Attach a CV<input name="cv" type="file" hidden onchange="report('cv ' + this.files.length)"></label></p>
<p><input id="hidden-pick" name="script" type="file" style="display:none"><button type="button" onclick="document.getElementById('hidden-pick').click()">Choose a file</button></p>
<button>Send</button>
</form></main>`,
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
	// Page strings that would break a line: a "<!--" in the title and the
	// text, and newlines in an iframe's malformed src, a button's name and
	// a big navigation's name (PROJ-95).
	"/probe-lines": `<!doctype html><title>Lines &lt;!-- title</title>
<main><p>code sample: &lt;!-- x LINES-AFTER</p>
<p>LINES-NEXT-PARA</p>
<iframe src="http://127.0.0.1:9/%zz&#10;# FAKE-HEADING&#10;e99 button &quot;FAKE-REF&quot;" width=50 height=50></iframe>
<button aria-label="LINES-BUTTON&#10;# FAKE-NAME">x</button>
<nav aria-label="LINES-NAV&#10;# FAKE-NAV">` + strings.Repeat(`<a href="/x">Item</a> `, 25) + `</nav>
<p>LINES-END</p></main>`,
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
	// PROJ-116: links that download, as large blocks so a Visual-mode
	// click can't miss them, and a button that downloads nothing.
	"/downloads": `<!doctype html><title>Downloads</title>` + report + `
<body style="margin:0">
<a href="/export.csv" style="position:fixed;left:0;top:0;width:100%;height:40vh;display:block;background:#cde">Export CSV</a>
<a href="/big.csv" style="position:fixed;left:0;top:45vh;width:100%;height:20vh;display:block;background:#dce">Big file</a>
<button onclick="report('nothing clicked')" style="position:fixed;left:0;top:70vh;width:100%;height:20vh">Nothing</button>
</body>`,
	// A page with a quarter of a million elements.
	"/huge": `<!doctype html><title>Huge</title><div id="d"></div><script>
const d = document.getElementById('d'); const f = document.createDocumentFragment();
for (let i = 0; i < 250000; i++) { const s = document.createElement('i'); f.appendChild(s); }
d.appendChild(f);</script>`,
	// PROJ-124: controls built from a div or a span, which only a pointer
	// cursor or a tabindex give away; a decorative pointer div with no
	// text; a link whose children inherit its pointer; a pointer div
	// holding a real button; and a span whose click a listener on the
	// document handles, as frameworks do, which leaves it out of
	// Chromium's accessibility tree.
	"/divs": `<!doctype html><title>Clickable divs</title>` + report + `
<main><h1>Inbox</h1>
<div style="cursor:pointer" onclick="report('a')"><span>Archive</span> <span>(3)</span></div>
<p><span tabindex="0" onclick="report('b')">Mark read</span></p>
<p>Liked by 2 people. <span class="star" style="cursor:pointer">Star it</span></p>
<div style="cursor:pointer;width:40px;height:20px;background:#ccc" onclick="report('decor')"></div>
<p><a href="#more" onclick="report('link')"><span>More</span> <b>options</b></a></p>
<div style="cursor:pointer;padding:4px" onclick="report('card')"><button onclick="report('save')">Save</button></div>
</main>
<script>document.addEventListener('click', e => { if (e.target.closest('.star')) report('c'); });</script>`,
	// Buttons inside a closed shadow root: one free, one under a cover.
	"/shadow": `<!doctype html><title>Shadow</title>` + report + `
<div id="host"></div>
<script>
const root = document.getElementById('host').attachShadow({mode: 'closed'});
root.innerHTML = '<button onclick="report(\'shadow free clicked\')">Free in shadow</button>' +
  '<span style="position:relative;display:inline-block;margin-left:20px"><button onclick="report(\'shadow covered clicked\')">Covered in shadow</button>' +
  '<span aria-label="Shadow cover" role="img" style="position:absolute;inset:0"></span></span>';
</script>`,
	// A Cloudflare challenge page, as far as BrowserPier looks (PROJ-120):
	// its title and #challenge-stage, and in it a 300×65 widget holding
	// the hidden response input, shaped as Turnstile shapes it. A trusted
	// click reports where it landed and where the checkbox's middle is
	// (21 px in, halfway down the first 65 px), in CSS pixels.
	"/cf-challenge": `<!doctype html><title>Just a moment...</title>` + report + `
<style>body{margin:0;font-family:sans-serif} #w{position:absolute;left:240px;top:260px;width:300px;height:65px;box-sizing:border-box;border:1px solid #d9d9d9;background:#fafafa}
#box{position:absolute;left:9px;top:21px;width:21px;height:21px;box-sizing:border-box;border:2px solid #666;background:#fff}</style>
<h1>shop.example</h1><p>Verify you are human by completing the action below.</p>
<div id="challenge-stage"><div id="w"><input type="hidden" name="cf-turnstile-response" id="cf-chl-widget-abc12_response"><div id="box"></div></div></div>
<script>
document.addEventListener('click', e => {
  if (!e.isTrusted) return;
  const r = document.getElementById('w').getBoundingClientRect();
  report('cf-click ' + [e.clientX, e.clientY, r.left + 21, r.top + Math.min(r.height, 65) / 2].join(' '));
});
</script>`,
	// A site's own sign-in with a Turnstile in its form, as Turnstile
	// renders it (the same id shape as the challenge page's widget, in a
	// laid-out box where its iframe would be), on a
	// page that also has the JavaScript-detections script Cloudflare adds
	// to ordinary pages: not a challenge page, so no Cloudflare note
	// (PROJ-120).
	"/turnstile-login": `<!doctype html><title>Sign in</title>
<script src="/cdn-cgi/challenge-platform/scripts/jsd/main.js"></script>
<main><h1>Sign in</h1>
<form method="post" action="/login">
<label for="email">Email</label> <input id="email" name="email" autocomplete="username">
<label for="password">Password</label> <input id="password" name="password" type="password" autocomplete="current-password">
<div class="cf-turnstile"><div style="width:300px;height:65px;border:1px solid #d9d9d9"><input type="hidden" name="cf-turnstile-response" id="cf-chl-widget-xk3q9_response"></div></div>
<button>Sign in</button>
</form></main>`,
	// A challenge page whose widget has been answered (its input has a
	// value), unnamed as an older challenge page's: no note either.
	"/cf-challenge-passed": `<!doctype html><title>Just a moment...</title>
<div id="challenge-form"><div style="width:300px;height:65px"><input type="hidden" id="cf-chl-widget-def34" value="0.token"></div></div>`,
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

// ExportCSV is what /export.csv downloads; /big.csv is BigSize bytes,
// sent over 5 s.
const (
	ExportCSV = "name,amount\nana,12\nbo,7\n"
	BigSize   = 5_000_000
)

// SlowFetch is how long /slow takes to answer: /spa-load fetches it three
// times in a row after its load.
const SlowFetch = 400 * time.Millisecond

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
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		mr, err := r.MultipartReader()
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var b strings.Builder
		b.WriteString(`<!doctype html><title>Uploaded</title><h1>Uploaded</h1><ul>`)
		for {
			part, err := mr.NextPart()
			if err != nil {
				break
			}
			if part.FileName() == "" {
				continue
			}
			h := sha256.New()
			n, _ := io.Copy(h, part)
			u := Upload{Field: part.FormName(), Name: part.FileName(), Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
			s.mu.Lock()
			s.uploads = append(s.uploads, u)
			s.mu.Unlock()
			fmt.Fprintf(&b, "<li>%s: %s, %d bytes, sha256 %s</li>", html.EscapeString(u.Field), html.EscapeString(u.Name), u.Size, u.SHA256)
		}
		b.WriteString("</ul>")
		_, _ = io.WriteString(w, b.String())
	})
	mux.HandleFunc("GET /redirect", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/login", http.StatusFound)
	})
	// PROJ-125: echo p in the title, the text and a link, as a careless
	// site echoes a password it got in its URL; /bounce redirects there.
	mux.HandleFunc("GET /echo", func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Query().Get("p")
		_, _ = fmt.Fprintf(w, `<!doctype html><title>Welcome %s</title><h1>Signed in</h1><p>You said: %s</p><p><a href="/echo?p=%s">Again</a></p>`,
			html.EscapeString(p), html.EscapeString(p), url.QueryEscape(p))
	})
	mux.HandleFunc("GET /bounce", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/echo?p="+url.QueryEscape(r.URL.Query().Get("p")), http.StatusFound)
	})
	mux.HandleFunc("GET /redirect-fail", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://169.254.169.254/sso", http.StatusFound)
	})
	// PROJ-116: a small CSV, as an attachment, and a 5 MB one that takes
	// 5 s to arrive. Both are CSV: Chrome holds a download of a type it
	// warns about from a plain-HTTP site (a .bin, say) as "Unconfirmed
	// N.crdownload" until a person keeps it, which would never finish.
	mux.HandleFunc("GET /export.csv", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="export.csv"`)
		w.Header().Set("Content-Type", "text/csv")
		_, _ = io.WriteString(w, ExportCSV)
	})
	mux.HandleFunc("GET /big.csv", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Disposition", `attachment; filename="big.csv"`)
		w.Header().Set("Content-Type", "text/csv")
		w.Header().Set("Content-Length", strconv.Itoa(BigSize))
		chunk := []byte(strings.Repeat("1,2\n", BigSize/50/4))
		for range 50 {
			if _, err := w.Write(chunk); err != nil {
				return
			}
			if fl, ok := w.(http.Flusher); ok {
				fl.Flush()
			}
			select {
			case <-r.Context().Done():
				return
			case <-time.After(100 * time.Millisecond):
			}
		}
	})
	// PROJ-122: an answer 400 ms late, and one that never comes.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(SlowFetch):
			w.WriteHeader(http.StatusNoContent)
		}
	})
	mux.HandleFunc("GET /hang", func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() })
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
