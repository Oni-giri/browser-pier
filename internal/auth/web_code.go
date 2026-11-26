package auth

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sync"
	"time"
)

// The second step of a sign-in, when two-step sign-in is on: a right
// password leaves a pending step, held in memory by its hash and named by
// a cookie on /login only; a right code turns it into a session.
const (
	stepTTL      = 5 * time.Minute
	maxSteps     = 64 // pending steps in memory; the oldest goes first
	maxStepFails = 5  // wrong codes that end a pending step
)

type pendingStep struct {
	hash     string // SHA-256 of the cookie
	proof    string // of the password step, for finishSignIn
	next     string
	remember bool
	made     time.Time
	fails    int
}

type pendingSteps struct {
	mu  sync.Mutex
	m   map[string]*pendingStep
	now func() time.Time
}

func newPendingSteps() *pendingSteps {
	return &pendingSteps{m: map[string]*pendingStep{}, now: time.Now}
}

// add keeps p and returns the cookie value that names it.
func (ps *pendingSteps) add(p *pendingStep) string {
	token := randomString(32)
	p.hash = hashToken(token)
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p.made = ps.now()
	ps.keepLocked(p)
	return token
}

// take removes and returns the step token names, if it is still valid.
// A step is in one request at a time: a second one in parallel finds
// nothing.
func (ps *pendingSteps) take(token string) *pendingStep {
	if token == "" {
		return nil
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	h := hashToken(token)
	p := ps.m[h]
	delete(ps.m, h)
	if p == nil || ps.now().Sub(p.made) >= stepTTL {
		return nil
	}
	return p
}

// peek returns where the step token names goes next, if it is valid.
func (ps *pendingSteps) peek(token string) (string, bool) {
	if token == "" {
		return "", false
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	p := ps.m[hashToken(token)]
	if p == nil || ps.now().Sub(p.made) >= stepTTL {
		return "", false
	}
	return p.next, true
}

// put gives back a step that take returned and that may go on.
func (ps *pendingSteps) put(p *pendingStep) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if ps.now().Sub(p.made) < stepTTL {
		ps.keepLocked(p)
	}
}

func (ps *pendingSteps) keepLocked(p *pendingStep) {
	now := ps.now()
	for h, q := range ps.m {
		if now.Sub(q.made) >= stepTTL {
			delete(ps.m, h)
		}
	}
	for len(ps.m) >= maxSteps {
		var oldest *pendingStep
		for _, q := range ps.m {
			if oldest == nil || q.made.Before(oldest.made) {
				oldest = q
			}
		}
		delete(ps.m, oldest.hash)
	}
	ps.m[p.hash] = p
}

func stepToken(r *http.Request) string {
	if c, err := r.Cookie(StepCookie); err == nil {
		return c.Value
	}
	return ""
}

func (w *Web) clearStepCookie(rw http.ResponseWriter, r *http.Request) {
	//nolint:gosec // as the step cookie
	http.SetCookie(rw, &http.Cookie{Name: StepCookie, Value: "", Path: "/login", MaxAge: -1, HttpOnly: true, Secure: w.SecureCookie(r), SameSite: http.SameSiteStrictMode})
}

func codePageData(next string) pageData { return pageData{Title: "Enter your code", Next: next} }

// codePage asks for the code, while a pending step is valid; otherwise
// it sends the browser back to the password.
func (w *Web) codePage(rw http.ResponseWriter, r *http.Request) {
	if w.admin.NeedsSetup() {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return
	}
	next, ok := w.steps.peek(stepToken(r))
	if !ok {
		http.Redirect(rw, r, "/login", http.StatusSeeOther)
		return
	}
	w.render(rw, r, http.StatusOK, "code", codePageData(next))
}

// loginCode checks the second step. Wrong codes count per pending step
// (5 end it), per address (5 in 5 minutes lock code entry there for 5
// minutes), and, for app codes from browsers without a trusted device
// cookie, for the whole account (10 a day). A trusted device's wrong
// codes count on its device cookie instead. A right password never
// clears any of these.
func (w *Web) loginCode(rw http.ResponseWriter, r *http.Request) {
	if w.admin.NeedsSetup() {
		http.Redirect(rw, r, "/setup", http.StatusSeeOther)
		return
	}
	ip := w.ClientIP(r)
	d := codePageData("/")
	mine, wait := w.codeIP.Begin(ip)
	if mine == nil {
		w.codeLocked(rw, r, d, wait)
		return
	}
	if err := readForm(rw, r); err != nil {
		mine.Cancel()
		http.Error(rw, "bad form", http.StatusBadRequest)
		return
	}
	p := w.steps.take(stepToken(r))
	if p == nil {
		// No step, someone else's, expired or ended: checked nothing.
		mine.Cancel()
		w.clearStepCookie(rw, r)
		http.Redirect(rw, r, "/login", http.StatusSeeOther)
		return
	}
	d.Next = p.next
	code, kind := classifyCode(r.PostFormValue("code"))

	var device *Attempt
	if c, err := r.Cookie(DeviceCookie); err == nil && w.admin.KnownDevice(r.Context(), c.Value) {
		device, _ = w.deviceFails.Begin(c.Value)
	}
	var account *Attempt
	if kind == codeTOTP && device == nil {
		var wait time.Duration
		account, wait = w.codeAccount.Begin("account")
		if account == nil {
			mine.Cancel()
			w.steps.put(p)
			retryAfter(rw, wait)
			if wait <= time.Second { // only attempts in flight fill it
				d.Error = "Too many sign-in attempts right now. Try again in a moment."
			} else {
				w.log.Warn("admin sign-in code refused: too many wrong codes for the account today", "ip", ip)
				d.Error = "Too many wrong codes for this account today. Use a recovery code, or sign in from a browser you've used before."
			}
			w.render(rw, r, http.StatusTooManyRequests, "code", d)
			return
		}
	}
	cancelAll := func() {
		mine.Cancel()
		if device != nil {
			device.Cancel()
		}
		if account != nil {
			account.Cancel()
		}
	}

	oldCookie := ""
	if c, err := r.Cookie(CookieName); err == nil {
		oldCookie = c.Value
	}
	value, s, err := w.admin.finishSignIn(r.Context(), p.proof, code, kind, p.remember, r.UserAgent(), oldCookie)
	switch {
	case errors.Is(err, ErrWrongCode):
		left := mine.Fail()
		if device != nil {
			device.Fail()
		}
		if account != nil {
			account.Fail()
		}
		p.fails++
		stepLeft := maxStepFails - p.fails
		w.log.Warn("wrong sign-in code after a right password", "ip", ip, "tries_left", min(left, stepLeft))
		switch {
		case left == 0:
			w.clearStepCookie(rw, r)
			w.codeLocked(rw, r, d, 5*time.Minute)
		case stepLeft <= 0:
			w.clearStepCookie(rw, r)
			w.render(rw, r, http.StatusUnauthorized, "login", pageData{Title: "Sign in", Next: p.next, EnvManaged: w.admin.EnvManaged(),
				Error: "Too many wrong codes. Enter your password again."})
		default:
			w.steps.put(p)
			n := min(left, stepLeft)
			d.Error = fmt.Sprintf("That code isn't right. %d %s left.", n, plural(n, "try", "tries"))
			w.render(rw, r, http.StatusUnauthorized, "code", d)
		}
		return
	case errors.Is(err, ErrSignInAgain):
		cancelAll()
		w.clearStepCookie(rw, r)
		http.Redirect(rw, r, "/login?next="+url.QueryEscape(p.next), http.StatusSeeOther)
		return
	case errors.Is(err, ErrTwoStepUnreadable):
		cancelAll()
		w.clearStepCookie(rw, r)
		w.signInFailed(rw, r, pageData{Title: "Sign in", Next: p.next, EnvManaged: w.admin.EnvManaged()}, err)
		return
	case err != nil:
		cancelAll()
		w.steps.put(p)
		w.log.Error("checking a sign-in code", "err", err)
		d.Error = "Something went wrong. Try again."
		w.render(rw, r, http.StatusInternalServerError, "code", d)
		return
	}
	mine.Succeed()
	if device != nil {
		device.Succeed()
	}
	if account != nil {
		account.Cancel() // the account's count never clears on a success
	}
	w.clearStepCookie(rw, r)
	w.setCookies(rw, r, value, s)
	w.log.Info("admin signed in", "ip", ip, "remember", p.remember, "second_step", kind.String())
	http.Redirect(rw, r, safeNext(p.next), http.StatusSeeOther)
}

func (w *Web) codeLocked(rw http.ResponseWriter, r *http.Request, d pageData, wait time.Duration) {
	retryAfter(rw, wait)
	if wait <= time.Second { // only tries in flight fill it, no lockout
		d.Error = "Too many sign-in attempts right now. Try again in a moment."
	} else {
		mins := int(math.Ceil(wait.Minutes()))
		d.Error = fmt.Sprintf("Too many wrong codes. Try again in %d %s.", mins, plural(mins, "minute", "minutes"))
	}
	w.render(rw, r, http.StatusTooManyRequests, "code", d)
}

// SettingsCode counts a code typed in Settings (to turn two-step off,
// replace the app or make new recovery codes) toward the account-wide
// cap on wrong app codes, as at sign-in: a stolen session plus the
// password then gets no more guesses at the code than a sign-in does.
// Recovery codes and other shapes aren't counted. ok false means the cap
// is reached; otherwise call done with whether the code was wrong.
func (w *Web) SettingsCode(code string) (done func(wrong bool), ok bool) {
	if _, kind := classifyCode(code); kind != codeTOTP {
		return func(bool) {}, true
	}
	a, _ := w.codeAccount.Begin("account")
	if a == nil {
		return nil, false
	}
	return func(wrong bool) {
		if wrong {
			a.Fail()
		} else {
			a.Cancel() // never cleared by a success
		}
	}, true
}
