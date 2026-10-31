package auth

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// maxWaiting caps sign-ins queued behind the global throttle.
const maxWaiting = 64

// CookieName is the admin session cookie; DeviceCookie marks a browser
// that has signed in before.
const (
	CookieName   = "bp_session"
	DeviceCookie = "bp_device"
)

// WebOptions say how requests reach BrowserPier.
type WebOptions struct {
	// TrustedProxies may set X-Forwarded-For and X-Forwarded-Proto.
	TrustedProxies []*net.IPNet
	// CookieSecure is "auto" (Secure when served over HTTPS), "always" or "never".
	CookieSecure string
	// PublicURL, when https, makes cookies Secure under "auto".
	PublicURL string
}

// Web serves sign-in and setup, and guards routes: the session cookie
// for the UI, agent tokens for the API. The two never mix, so a page
// can't use the admin's cookie to call the API.
type Web struct {
	admin  *Admin
	tokens *Tokens
	log    *slog.Logger
	opts   WebOptions

	loginIP     *Lockout  // per client
	backstop    *backstop // failures from everywhere; past a limit, sign-ins are slowed
	deviceFails *Lockout  // per device cookie; a locked one is treated as unknown
	waiting     atomic.Int32
	slow        chan struct{}
	setup       *Lockout
	badToken    *Lockout
	pages       map[string]*template.Template
}

//go:embed pages.html
var pagesHTML string

func NewWeb(admin *Admin, tokens *Tokens, log *slog.Logger, opts WebOptions) *Web {
	w := &Web{
		admin: admin, tokens: tokens, log: log, opts: opts,
		loginIP:     NewLockout(5, 5*time.Minute, 5*time.Minute),
		deviceFails: NewLockout(5, time.Hour, 24*time.Hour),
		backstop:    &backstop{window: 15 * time.Minute, free: 20, base: 2 * time.Second, max: 30 * time.Second, now: time.Now},
		setup:       NewLockout(10, 5*time.Minute, 5*time.Minute),
		badToken:    NewLockout(20, 5*time.Minute, 5*time.Minute),
		slow:        make(chan struct{}, 1),
		pages:       map[string]*template.Template{},
	}
	base := template.Must(template.New("").Parse(pagesHTML))
	for _, name := range []string{"login", "setup"} {
		t := template.Must(base.Clone())
		template.Must(t.Parse(`{{define "body"}}{{template "` + name + `-body" .}}{{end}}`))
		w.pages[name] = t
	}
	return w
}

// Register adds /login, /logout and /setup.
func (w *Web) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /login", w.loginPage)
	mux.HandleFunc("POST /login", w.login)
	mux.HandleFunc("POST /logout", w.logout)
	mux.HandleFunc("GET /setup", w.setupPage)
	mux.HandleFunc("POST /setup", w.doSetup)
}

type ctxKey int

const (
	tokenKey ctxKey = iota
	adminKey
)

// TokenFromContext returns the agent token that authenticated the request.
func TokenFromContext(ctx context.Context) (Token, bool) {
	t, ok := ctx.Value(tokenKey).(Token)
	return t, ok
}

// SessionFromContext returns the admin session that authenticated the request.
func SessionFromContext(ctx context.Context) (AdminSession, bool) {
	s, ok := ctx.Value(adminKey).(AdminSession)
	return s, ok
}

// RequireAdmin lets through requests from a signed-in browser. Others go
// to the sign-in page (or setup, before there is a password).
func (w *Web) RequireAdmin(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if w.admin.NeedsSetup() {
			w.redirectToSignIn(rw, r, "/setup")
			return
		}
		c, err := r.Cookie(CookieName)
		if err != nil {
			w.redirectToSignIn(rw, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()))
			return
		}
		s, err := w.admin.Session(r.Context(), c.Value)
		if err != nil {
			if err != ErrNoSession { //nolint:errorlint // sentinel
				w.log.Error("checking an admin session", "err", err)
			}
			w.redirectToSignIn(rw, r, "/login?next="+url.QueryEscape(r.URL.RequestURI()))
			return
		}
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), adminKey, s)))
	})
}

func (w *Web) redirectToSignIn(rw http.ResponseWriter, r *http.Request, to string) {
	if r.Header.Get("HX-Request") == "true" {
		// htmx follows this header instead of swapping a login page into a fragment.
		rw.Header().Set("HX-Redirect", to)
		rw.WriteHeader(http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(rw, "sign in first", http.StatusUnauthorized)
		return
	}
	http.Redirect(rw, r, to, http.StatusSeeOther)
}

// RequireToken lets through requests with a valid agent token in
// "Authorization: Bearer". It never looks at cookies.
func (w *Web) RequireToken(next http.Handler) http.Handler {
	return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		ip := w.ClientIP(r)
		if wait := w.badToken.Locked(ip); wait > 0 {
			retryAfter(rw, wait)
			jsonError(rw, http.StatusTooManyRequests, "too many bad tokens from this address; try again later")
			return
		}
		secret, ok := bearer(r)
		if !ok {
			rw.Header().Set("WWW-Authenticate", `Bearer realm="BrowserPier"`)
			jsonError(rw, http.StatusUnauthorized, "send an agent token as Authorization: Bearer bp_live_…")
			return
		}
		t, err := w.tokens.Authenticate(r.Context(), secret)
		if err != nil {
			if err != ErrInvalidToken { //nolint:errorlint // sentinel
				w.log.Error("checking an agent token", "err", err)
				jsonError(rw, http.StatusInternalServerError, "couldn't check the token")
				return
			}
			// A 256-bit token can't be guessed, so counting in flight doesn't
			// matter here; the limit just stops one address hammering us.
			if a, _ := w.badToken.Begin(ip); a != nil {
				a.Fail()
			}
			w.log.Warn("agent token rejected", "ip", ip)
			rw.Header().Set("WWW-Authenticate", `Bearer realm="BrowserPier", error="invalid_token"`)
			jsonError(rw, http.StatusUnauthorized, ErrInvalidToken.Error())
			return
		}
		next.ServeHTTP(rw, r.WithContext(context.WithValue(r.Context(), tokenKey, t)))
	})
}

func bearer(r *http.Request) (string, bool) {
	scheme, secret, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	secret = strings.TrimSpace(secret)
	return secret, secret != ""
}

func jsonError(rw http.ResponseWriter, status int, msg string) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(map[string]string{"error": msg})
}

func retryAfter(rw http.ResponseWriter, d time.Duration) {
	rw.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(d.Seconds()))))
}

// ClientIP is the address the request came from: the connection's peer,
// or, when that peer is a trusted proxy, the last untrusted hop it reports
// in X-Forwarded-For.
func (w *Web) ClientIP(r *http.Request) string {
	peer := remoteIP(r)
	if !w.trusted(peer) {
		return peer.String()
	}
	hops := strings.Split(strings.Join(r.Header.Values("X-Forwarded-For"), ","), ",")
	for i := len(hops) - 1; i >= 0; i-- {
		ip := net.ParseIP(strings.TrimSpace(hops[i]))
		if ip == nil {
			break
		}
		if !w.trusted(ip) || i == 0 {
			return ip.String()
		}
	}
	return peer.String()
}

// IsHTTPS reports whether the client reached us over HTTPS, directly or
// through a trusted proxy that says so.
func (w *Web) IsHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	return w.trusted(remoteIP(r)) && strings.EqualFold(strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")), "https")
}

func (w *Web) secureCookie(r *http.Request) bool {
	switch w.opts.CookieSecure {
	case "always":
		return true
	case "never":
		return false
	}
	return w.IsHTTPS(r) || strings.HasPrefix(w.opts.PublicURL, "https://")
}

func remoteIP(r *http.Request) net.IP {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return net.IPv4zero
	}
	return ip
}

func (w *Web) trusted(ip net.IP) bool {
	for _, n := range w.opts.TrustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// safeNext keeps a post-login redirect on this site: a path, nothing a
// browser could read as another host. Browsers drop tabs and newlines
// from URLs and treat a backslash as a slash, so any of those is refused
// outright rather than cleaned up.
func safeNext(next string) string {
	for _, c := range next {
		if c < 0x20 || c == 0x7f || c == '\\' {
			return "/"
		}
	}
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") {
		return "/"
	}
	u, err := url.Parse(next)
	if err != nil || u.Scheme != "" || u.Host != "" || u.User != nil || u.Opaque != "" {
		return "/"
	}
	return next
}

type pageData struct {
	Title      string
	Error      string
	Next       string
	Remember   bool
	EnvManaged bool
}

func (w *Web) render(rw http.ResponseWriter, status int, name string, d pageData) {
	rw.Header().Set("Content-Type", "text/html; charset=utf-8")
	rw.Header().Set("Cache-Control", "no-store")
	rw.Header().Set("X-Frame-Options", "DENY")
	rw.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'")
	rw.WriteHeader(status)
	if err := w.pages[name].ExecuteTemplate(rw, "page", d); err != nil {
		w.log.Error("rendering a page", "page", name, "err", err)
	}
}

func (w *Web) loginPage(rw http.ResponseWriter, r *http.Request) {
	if w.admin.NeedsSetup() {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return
	}
	next := safeNext(r.URL.Query().Get("next"))
	if c, err := r.Cookie(CookieName); err == nil {
		if _, err := w.admin.Session(r.Context(), c.Value); err == nil {
			http.Redirect(rw, r, next, http.StatusSeeOther) //nolint:gosec // next went through safeNext
			return
		}
	}
	w.render(rw, http.StatusOK, "login", pageData{Title: "Sign in", Next: next, EnvManaged: w.admin.EnvManaged()})
}

func (w *Web) login(rw http.ResponseWriter, r *http.Request) {
	if w.admin.NeedsSetup() {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return
	}
	// Count the attempt before reading the body, so slow bodies are
	// limited per address like any other try.
	ip := w.ClientIP(r)
	mine, wait := w.loginIP.Begin(ip)
	if mine == nil {
		w.locked(rw, pageData{Title: "Sign in", Next: "/", EnvManaged: w.admin.EnvManaged()}, wait)
		return
	}
	if err := readForm(rw, r); err != nil {
		mine.Cancel()
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	d := pageData{Title: "Sign in", Next: safeNext(r.PostFormValue("next")), Remember: r.PostFormValue("remember") == "1", EnvManaged: w.admin.EnvManaged()}

	// After many failures from everywhere, sign-ins from unknown devices go
	// one at a time, each after a delay that grows with the failures: an
	// attacker spread over many addresses gets a few thousand guesses a day
	// at most, and the owner's known devices aren't slowed at all.
	// A device cookie that keeps failing (stolen, say) stops being trusted.
	var device *Attempt
	if c, err := r.Cookie(DeviceCookie); err == nil && w.admin.KnownDevice(r.Context(), c.Value) {
		device, _ = w.deviceFails.Begin(c.Value)
	}
	var global *backstopAttempt
	if device == nil {
		var delay time.Duration
		global, delay = w.backstop.begin()
		defer global.end()
		if delay > 0 {
			release, ok := w.throttle(r.Context(), delay)
			if !ok {
				mine.Cancel()
				retryAfter(rw, time.Minute)
				d.Error = "Too many sign-in attempts right now. Try again in a minute."
				w.render(rw, http.StatusTooManyRequests, "login", d)
				return
			}
			defer release()
		}
	}
	oldCookie := ""
	if c, err := r.Cookie(CookieName); err == nil {
		oldCookie = c.Value
	}
	value, s, err := w.admin.SignIn(r.Context(), r.PostFormValue("password"), d.Remember, r.UserAgent(), oldCookie)
	if errors.Is(err, ErrWrongPassword) {
		left := mine.Fail()
		if global != nil {
			global.fail()
		}
		if device != nil {
			device.Fail()
		}
		w.log.Warn("admin sign-in failed", "ip", ip, "tries_left", left)
		if left == 0 {
			w.locked(rw, d, 5*time.Minute)
			return
		}
		d.Error = fmt.Sprintf("Wrong password. %d %s left before a 5‑minute lockout.", left, plural(left, "try", "tries"))
		w.render(rw, http.StatusUnauthorized, "login", d)
		return
	}
	if err != nil {
		mine.Cancel()
		if device != nil {
			device.Cancel()
		}
		w.log.Error("signing in", "err", err)
		d.Error = "Something went wrong. Try again."
		w.render(rw, http.StatusInternalServerError, "login", d)
		return
	}
	mine.Succeed()
	if device != nil {
		device.Succeed()
	}
	w.setCookies(rw, r, value, s)
	w.log.Info("admin signed in", "ip", ip, "remember", d.Remember)
	http.Redirect(rw, r, d.Next, http.StatusSeeOther)
}

// readForm parses a small form with a deadline, so a client can't hold a
// connection open by sending its body slowly.
func readForm(rw http.ResponseWriter, r *http.Request) error {
	_ = http.NewResponseController(rw).SetReadDeadline(time.Now().Add(15 * time.Second))
	r.Body = http.MaxBytesReader(rw, r.Body, 8<<10)
	return r.ParseForm()
}

// throttle waits for the one sign-in slot and then for d. The caller
// holds the slot through its password check, until it calls release. It
// waits as long as the client does, and returns false if it gives up.
func (w *Web) throttle(ctx context.Context, d time.Duration) (release func(), ok bool) {
	// At most maxWaiting attempts queue at once; the rest are told to come
	// back, which keeps memory bounded. Known devices never queue.
	if w.waiting.Add(1) > maxWaiting {
		w.waiting.Add(-1)
		return nil, false
	}
	defer w.waiting.Add(-1)
	select {
	case w.slow <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	release = func() { <-w.slow }
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return release, true
	case <-ctx.Done():
		release()
		return nil, false
	}
}

// backstop counts sign-in failures from every address, and attempts in
// flight. Past free of them within window, each attempt is delayed by
// base, doubling with every further free, up to max.
type backstop struct {
	window    time.Duration
	free      int
	base, max time.Duration
	now       func() time.Time
	mu        sync.Mutex
	fails     []time.Time
	pending   int
}

type backstopAttempt struct {
	b    *backstop
	done bool
}

// begin counts an attempt from the start, so parallel requests can't all
// see a zero delay before any of them has failed.
func (b *backstop) begin() (*backstopAttempt, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.fails = prune(b.fails, b.now().Add(-b.window))
	d := b.delayLocked(len(b.fails) + b.pending)
	b.pending++
	return &backstopAttempt{b: b}, d
}

func (t *backstopAttempt) fail() {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	now := t.b.now()
	t.b.fails = append(prune(t.b.fails, now.Add(-t.b.window)), now)
	t.finishLocked()
}

// end finishes the attempt if fail didn't.
func (t *backstopAttempt) end() {
	t.b.mu.Lock()
	defer t.b.mu.Unlock()
	t.finishLocked()
}

func (t *backstopAttempt) finishLocked() {
	if !t.done {
		t.done = true
		t.b.pending--
	}
}

func (b *backstop) delayLocked(n int) time.Duration {
	over := n - b.free
	if over < 0 {
		return 0
	}
	d := b.base
	for i := 0; i < over/b.free && d < b.max; i++ {
		d *= 2
	}
	return min(d, b.max)
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func (w *Web) locked(rw http.ResponseWriter, d pageData, wait time.Duration) {
	retryAfter(rw, wait)
	mins := int(math.Ceil(wait.Minutes()))
	d.Error = fmt.Sprintf("Too many wrong passwords. Try again in %d %s.", mins, plural(mins, "minute", "minutes"))
	w.render(rw, http.StatusTooManyRequests, "login", d)
}

// setCookies hands the browser its new session cookie, and a device
// cookie that marks it as known for future sign-ins.
func (w *Web) setCookies(rw http.ResponseWriter, r *http.Request, value string, s AdminSession) {
	//nolint:gosec // Secure is set whenever served over HTTPS; plain-HTTP private setups can't store a Secure cookie
	c := &http.Cookie{
		Name:     CookieName,
		Value:    value,
		Path:     "/",
		HttpOnly: true,
		Secure:   w.secureCookie(r),
		SameSite: http.SameSiteLaxMode,
	}
	if s.Remember {
		c.Expires = s.ExpiresAt
	}
	http.SetCookie(rw, c)
	if device, err := w.admin.NewDeviceToken(r.Context()); err == nil {
		//nolint:gosec // as above
		http.SetCookie(rw, &http.Cookie{
			Name: DeviceCookie, Value: device, Path: "/login", HttpOnly: true,
			Secure: c.Secure, SameSite: http.SameSiteLaxMode, Expires: time.Now().Add(DeviceTTL),
		})
	}
}

func (w *Web) logout(rw http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		if err := w.admin.EndSession(r.Context(), c.Value); err != nil {
			w.log.Error("signing out", "err", err)
		}
	}
	//nolint:gosec // as in signIn
	http.SetCookie(rw, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: w.secureCookie(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(rw, r, "/login", http.StatusSeeOther)
}

func (w *Web) setupPage(rw http.ResponseWriter, r *http.Request) {
	if !w.admin.NeedsSetup() {
		http.NotFound(rw, r)
		return
	}
	w.render(rw, http.StatusOK, "setup", pageData{Title: "Set up"})
}

func (w *Web) doSetup(rw http.ResponseWriter, r *http.Request) {
	if !w.admin.NeedsSetup() {
		http.NotFound(rw, r)
		return
	}
	d := pageData{Title: "Set up"}
	attempt, wait := w.setup.Begin("setup")
	if attempt == nil {
		retryAfter(rw, wait)
		d.Error = "Too many wrong setup codes. Wait a few minutes, or restart BrowserPier for a new code."
		w.render(rw, http.StatusTooManyRequests, "setup", d)
		return
	}
	if err := readForm(rw, r); err != nil {
		attempt.Cancel()
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	password := r.PostFormValue("password")
	if password != r.PostFormValue("confirm") {
		attempt.Cancel()
		d.Error = "The two passwords don't match."
		w.render(rw, http.StatusBadRequest, "setup", d)
		return
	}
	err := w.admin.Setup(r.Context(), strings.TrimSpace(r.PostFormValue("code")), password)
	switch {
	case err == ErrWrongSetupCode: //nolint:errorlint // sentinel
		attempt.Fail()
		w.log.Warn("wrong setup code", "ip", w.ClientIP(r))
		d.Error = "That setup code is wrong. Copy it from BrowserPier's log."
		w.render(rw, http.StatusUnauthorized, "setup", d)
		return
	case err == ErrAlreadySetUp: //nolint:errorlint // sentinel
		attempt.Cancel()
		http.Redirect(rw, r, "/login", http.StatusSeeOther)
		return
	case err != nil:
		attempt.Cancel()
		d.Error = "Password: " + err.Error() + "."
		w.render(rw, http.StatusBadRequest, "setup", d)
		return
	}
	attempt.Succeed()
	value, s, err := w.admin.SignIn(r.Context(), password, false, r.UserAgent(), "")
	if err != nil {
		// The password is set; only the automatic sign-in failed.
		w.log.Error("signing in after setup", "err", err)
		http.Redirect(rw, r, "/login", http.StatusSeeOther)
		return
	}
	w.setCookies(rw, r, value, s)
	http.Redirect(rw, r, "/", http.StatusSeeOther)
}
