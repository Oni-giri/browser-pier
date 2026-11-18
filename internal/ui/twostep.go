package ui

import (
	"errors"
	"html/template"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
)

// Two-step sign-in in Settings → Admin account (PROJ-162). Turning it on
// asks the password, then shows a new secret (QR code and text) that
// does nothing until a code from it is right; the recovery codes follow,
// shown once in that response's body. Turning it off, replacing the app
// and new recovery codes ask the password and a code. Nothing here is
// reachable with an agent token, and nothing secret goes in a URL, a
// redirect or a flash message.

// draftTTL is how long a new secret waits for its first code.
const draftTTL = 10 * time.Minute

type twoStepDraft struct {
	secret  []byte
	made    time.Time
	replace bool   // replacing the app: the password and a code were checked
	mark    string // of the secret they were checked against, when replacing
}

// twoStepForm is the two-step dialog's content.
type twoStepForm struct {
	// Stage is "password" (turn on), "manage" (when on), "scan" (the new
	// secret) or "codes" (the recovery codes, once).
	Stage   string
	Replace bool
	Error   string
	QR      template.HTML
	Secret  string // grouped by four for reading
	Account string
	Codes   []string
}

func (u *UI) twoStepDialog(w http.ResponseWriter, status int, f twoStepForm) {
	u.fragment(w, status, "settings", "twostep-dialog", f)
}

func (u *UI) sessionID(r *http.Request) string {
	s, _ := auth.SessionFromContext(r.Context())
	return s.ID
}

func (u *UI) keepDraft(r *http.Request, d twoStepDraft) {
	u.twoStepMu.Lock()
	defer u.twoStepMu.Unlock()
	now := u.now()
	for k, v := range u.twoStepDrafts {
		if now.Sub(v.made) >= draftTTL {
			delete(u.twoStepDrafts, k)
		}
	}
	d.made = now
	u.twoStepDrafts[u.sessionID(r)] = d
}

// draft returns this admin session's new secret, if one is waiting.
func (u *UI) draft(r *http.Request) (twoStepDraft, bool) {
	u.twoStepMu.Lock()
	defer u.twoStepMu.Unlock()
	id := u.sessionID(r)
	d, ok := u.twoStepDrafts[id]
	if !ok || id == "" {
		return twoStepDraft{}, false
	}
	if u.now().Sub(d.made) >= draftTTL {
		delete(u.twoStepDrafts, id)
		return twoStepDraft{}, false
	}
	return d, true
}

func (u *UI) dropDraft(r *http.Request) {
	u.twoStepMu.Lock()
	defer u.twoStepMu.Unlock()
	delete(u.twoStepDrafts, u.sessionID(r))
}

// accountHost tells this instance apart in the authenticator app: the
// public URL's host, else the address the page was loaded from.
func (u *UI) accountHost(r *http.Request) string {
	if p, err := url.Parse(u.o.PublicURL); err == nil && p.Host != "" {
		return p.Host
	}
	return r.Host
}

// scanForm shows a draft's secret, as a QR code and as text.
func (u *UI) scanForm(r *http.Request, d twoStepDraft) (twoStepForm, error) {
	host := u.accountHost(r)
	qr, err := qrSVG(auth.TOTPURL(d.secret, host))
	if err != nil {
		return twoStepForm{}, err
	}
	enc := auth.EncodeTOTPSecret(d.secret)
	var groups []string
	for i := 0; i < len(enc); i += 4 {
		groups = append(groups, enc[i:min(i+4, len(enc))])
	}
	return twoStepForm{Stage: "scan", Replace: d.replace, QR: qr, Secret: strings.Join(groups, " "), Account: "BrowserPier: " + host}, nil
}

func (u *UI) showScan(w http.ResponseWriter, r *http.Request, status int, d twoStepDraft, msg string) {
	f, err := u.scanForm(r, d)
	if err != nil {
		u.log.Error("drawing the QR code", "err", err)
		u.twoStepDialog(w, http.StatusInternalServerError, twoStepForm{Stage: "password", Error: "Something went wrong."})
		return
	}
	f.Error = msg
	u.twoStepDialog(w, status, f)
}

// twoStepOn checks the password and draws a new secret, stored nowhere
// until a code from it is right.
func (u *UI) twoStepOn(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	admin := u.web.Admin()
	st, err := admin.TwoStep(r.Context())
	switch {
	case err != nil:
		u.log.Error("reading two-step sign-in", "err", err)
		u.twoStepDialog(w, http.StatusInternalServerError, twoStepForm{Stage: "password", Error: "Two-step sign-in can't be read. Reset it from the server."})
		return
	case st.On:
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "manage", Error: "Two-step sign-in is already on."})
		return
	}
	ok, msg := u.checkCurrent(r, func() (bool, error) { return admin.CheckPassword(r.Context(), r.PostFormValue("current")) })
	if !ok {
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "password", Error: msg})
		return
	}
	d := twoStepDraft{secret: auth.NewTOTPSecret()}
	u.keepDraft(r, d)
	u.showScan(w, r, http.StatusOK, d, "")
}

// twoStepConfirm stores the new secret once a code from it is right.
func (u *UI) twoStepConfirm(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	d, ok := u.draft(r)
	if !ok {
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "password", Error: "That took more than 10 minutes. Start again."})
		return
	}
	keep := ""
	if c, err := r.Cookie(auth.CookieName); err == nil {
		keep = c.Value
	}
	admin := u.web.Admin()
	code := r.PostFormValue("code")
	var codes []string
	var err error
	if d.replace {
		err = admin.ReplaceTwoStepApp(r.Context(), d.mark, d.secret, code, keep)
	} else {
		codes, err = admin.EnableTwoStep(r.Context(), d.secret, code, keep)
	}
	switch {
	case errors.Is(err, auth.ErrWrongCode):
		u.showScan(w, r, http.StatusUnprocessableEntity, d, "That code isn't right. Check the app shows BrowserPier, and try the next code.")
		return
	case errors.Is(err, auth.ErrTwoStepOn), errors.Is(err, auth.ErrTwoStepOff), errors.Is(err, auth.ErrSignInAgain):
		u.dropDraft(r)
		done(w, r, "/settings", "Two-step sign-in changed in the meantime. Look again", true)
		return
	case err != nil:
		u.log.Error("storing two-step sign-in", "err", err)
		u.showScan(w, r, http.StatusInternalServerError, d, "Something went wrong. Try again.")
		return
	}
	u.dropDraft(r)
	// The device key rotated: this browser stays known with a fresh cookie.
	u.web.SetDeviceCookie(w, r)
	if d.replace {
		u.log.Info("authenticator app replaced from the UI; other devices signed out")
		done(w, r, "/settings", "Authenticator app replaced. Other devices are signed out", false)
		return
	}
	u.log.Info("two-step sign-in turned on from the UI; other devices signed out")
	u.twoStepDialog(w, http.StatusOK, twoStepForm{Stage: "codes", Codes: codes})
}

// twoStepManage turns two-step off, starts replacing the app or makes
// new recovery codes: each with the password and a code.
func (u *UI) twoStepManage(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	action := r.PostFormValue("action")
	if action != "off" && action != "replace" && action != "codes" {
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	try, _ := u.pinTries.Begin(u.web.ClientIP(r))
	if try == nil {
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "manage", Error: "Too many wrong tries. Wait a few minutes."})
		return
	}
	admin := u.web.Admin()
	password, code := r.PostFormValue("current"), r.PostFormValue("code")
	// App codes typed here count on the account-wide cap too.
	counted, ok := u.web.SettingsCode(code)
	if !ok {
		try.Cancel()
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "manage", Error: "Too many wrong codes for this account today. Use a recovery code."})
		return
	}
	var codes []string
	var mark string
	var err error
	switch action {
	case "off":
		err = admin.DisableTwoStep(r.Context(), password, code)
	case "codes":
		codes, err = admin.NewRecoveryCodes(r.Context(), password, code)
	case "replace":
		mark, err = admin.ConfirmIdentity(r.Context(), password, code)
	}
	counted(errors.Is(err, auth.ErrWrongCode))
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		try.Fail()
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "manage", Error: "The current password isn't right."})
		return
	case errors.Is(err, auth.ErrWrongCode):
		try.Fail()
		u.twoStepDialog(w, http.StatusUnprocessableEntity, twoStepForm{Stage: "manage", Error: "That code isn't right. Use a current code from the app, or a recovery code."})
		return
	case errors.Is(err, auth.ErrTwoStepOff):
		try.Cancel()
		done(w, r, "/settings", "Two-step sign-in is already off", false)
		return
	case err != nil:
		try.Cancel()
		u.log.Error("changing two-step sign-in", "err", err)
		u.twoStepDialog(w, http.StatusInternalServerError, twoStepForm{Stage: "manage", Error: "Something went wrong."})
		return
	}
	try.Succeed()
	switch action {
	case "off":
		u.log.Info("two-step sign-in turned off from the UI")
		done(w, r, "/settings", "Two-step sign-in is off", false)
	case "codes":
		u.log.Info("new recovery codes made from the UI")
		u.twoStepDialog(w, http.StatusOK, twoStepForm{Stage: "codes", Codes: codes})
	case "replace":
		d := twoStepDraft{secret: auth.NewTOTPSecret(), replace: true, mark: mark}
		u.keepDraft(r, d)
		u.showScan(w, r, http.StatusOK, d, "")
	}
}
