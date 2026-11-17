package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/notify"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/settings"
	"github.com/Oni-giri/browser-pier/internal/theme"
)

type settingsData struct {
	settings.Settings
	Proxy        string // masked
	RAMPercent   int    // in effect
	RAMFromEnv   bool
	Mem          memoryView
	PINChangedAt time.Time
	Devices      int
	PasswordEnv  bool
	Error        string
	// RecordingsUsed is what session recordings hold now, in bytes.
	RecordingsUsed int64
	// Which form the error is for: pin, password, defaults or notify.
	ErrorFor string
	// Notify is the webhook as shown; nil when there is none to set.
	Notify *notify.View
	// NewSecret is the webhook's secret, shown once: just made by a first
	// save or by Regenerate.
	NewSecret string
}

// NotifyEvents are the webhook's events, with whether each is on.
func (d settingsData) NotifyEvents() []notifyEvent {
	if d.Notify == nil {
		return nil
	}
	var out []notifyEvent
	for _, k := range notify.Toggles {
		out = append(out, notifyEvent{Kind: k, Label: k.Label(), On: d.Notify.On[k]})
	}
	return out
}

type notifyEvent struct {
	Kind  notify.Kind
	Label string
	On    bool
}

// Policies are the handoff policies, least strict first.
func (settingsData) Policies() []policyChoice { return policyChoices }

type policyChoice struct {
	Value       profiles.HandoffPolicy
	Title, Desc string
	Hint        string
}

var policyChoices = []policyChoice{
	{profiles.PolicyLink, "One tap", "The link opens the browser. Fine for low-stakes logins.",
		"Anyone with the link can open the browser. Only over HTTPS on a public server."},
	{profiles.PolicyPIN, "Link + PIN", "Link plus your PIN. The agent never sees the PIN.",
		"The link asks for your handoff PIN first. A good default: a leaked link is useless on its own."},
	{profiles.PolicySignIn, "Full sign-in", "Your admin password. For email, banking and payments.",
		"The link asks for your admin password. For accounts where a mistake costs money."},
}

// AutoStopChoices are the idle timeouts offered, in minutes; 0 is never.
func (settingsData) AutoStopChoices() []int { return profiles.AutoStopChoices }

// Fits is how many browsers the limit holds at about 1 GB each, counting
// only memory other apps leave: what the host has free plus what the
// browsers already use.
func (d settingsData) Fits() int {
	if d.Mem.Each <= 0 {
		return 0
	}
	limit := d.Mem.Host * int64(d.RAMPercent) / 100
	if d.Mem.Free > 0 {
		limit = min(limit, d.Mem.Usable())
	}
	return int(limit / d.Mem.Each)
}

func (u *UI) settingsData(r *http.Request) (settingsData, error) {
	st, err := u.settings.Get(r.Context())
	if err != nil {
		return settingsData{}, err
	}
	d := settingsData{Settings: st, Proxy: maskProxy(st.DefaultProxy), RAMFromEnv: u.o.RAMLimitFromEnv,
		PasswordEnv: u.web.Admin().EnvManaged()}
	d.Mem = u.memoryView(r, nil)
	if u.o.Recordings != nil {
		d.RecordingsUsed, _ = u.o.Recordings.Usage()
	}
	d.RecordingRetentionDays = int(st.RecordingLimits().Retention / (24 * time.Hour))
	d.RecordingCapMiB = int(st.RecordingLimits().Cap >> 20)
	if d.Mem.Each == 0 {
		d.Mem.Each = 1 << 30
	}
	if d.Mem.Host > 0 {
		d.RAMPercent = int((d.Mem.Limit*100 + d.Mem.Host/2) / d.Mem.Host)
	}
	if !d.RAMFromEnv && st.RAMLimitPercent > 0 {
		d.RAMPercent = st.RAMLimitPercent
	}
	d.PINChangedAt, _ = u.web.Admin().PINChangedAt(r.Context())
	if all, err := u.web.Admin().Sessions(r.Context()); err == nil {
		d.Devices = len(all)
	}
	if u.o.Notify != nil {
		v := u.o.Notify.View()
		d.Notify = &v
	}
	return d, nil
}

func (u *UI) settingsPage(w http.ResponseWriter, r *http.Request) {
	d, err := u.settingsData(r)
	if err != nil {
		u.log.Error("reading settings", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	u.render(w, r, http.StatusOK, "settings", "Settings", "settings", d)
}

func (u *UI) settingsError(w http.ResponseWriter, r *http.Request, form, msg string) {
	d, err := u.settingsData(r)
	if err != nil {
		u.log.Error("reading settings", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	d.Error, d.ErrorFor = msg, form
	u.render(w, r, http.StatusUnprocessableEntity, "settings", "Settings", "settings", d)
}

// saveSettings saves the defaults for new profiles and the memory limit.
func (u *UI) saveSettings(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	st, err := u.settings.Get(r.Context())
	if err != nil {
		u.log.Error("reading settings", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	if v := r.PostFormValue("policy"); v != "" {
		st.DefaultPolicy = profiles.HandoffPolicy(v)
	}
	if v := r.PostFormValue("auto_stop"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			u.settingsError(w, r, "defaults", "Choose an auto-stop time.")
			return
		}
		st.DefaultAutoStop = time.Duration(n) * time.Minute
	}
	proxy := strings.TrimSpace(r.PostFormValue("proxy"))
	if proxy != "" && proxy == maskProxy(st.DefaultProxy) {
		proxy = st.DefaultProxy
	}
	st.DefaultProxy = proxy
	// A form without the checkbox (an older page) leaves it as it was.
	if r.PostFormValue("block_ads_field") == "1" {
		st.DefaultBlockAds = r.PostFormValue("block_ads") == "1"
	}
	for _, f := range []struct {
		name string
		to   *int
		msg  string
	}{
		{"rec_days", &st.RecordingRetentionDays, "Keep recordings for 1 to 90 days."},
		{"rec_cap_mb", &st.RecordingCapMiB, "Give recordings 64 MB or more."},
	} {
		if v := strings.TrimSpace(r.PostFormValue(f.name)); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				u.settingsError(w, r, "defaults", f.msg)
				return
			}
			*f.to = n
		}
	}
	ramChanged := false
	if v := r.PostFormValue("ram"); v != "" && !u.o.RAMLimitFromEnv {
		n, err := strconv.Atoi(v)
		if err != nil || n < 10 || n > 100 {
			u.settingsError(w, r, "defaults", "Set the memory limit between 10% and 100%.")
			return
		}
		ramChanged = n != st.RAMLimitPercent
		st.RAMLimitPercent = n
	}
	if err := u.settings.Save(r.Context(), st); err != nil {
		u.settingsError(w, r, "defaults", sentence(err.Error()))
		return
	}
	if u.o.Recordings != nil {
		u.o.Recordings.SetLimits(st.RecordingLimits())
		u.o.Recordings.Sweep() // a shorter retention, or less space, applies now
	}
	if ramChanged && u.o.SetRAMLimit != nil {
		u.o.SetRAMLimit(st.RAMLimitPercent)
		u.forget()
	}
	u.log.Info("settings saved from the UI")
	done(w, r, "/settings", "Settings saved", false)
}

func (u *UI) savePIN(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	admin := u.web.Admin()
	changed, err := admin.PINChangedAt(r.Context())
	if err != nil {
		u.log.Error("reading the PIN", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	if !changed.IsZero() {
		ok, msg := u.checkCurrent(r, func() (bool, error) { return admin.CheckPIN(r.Context(), r.PostFormValue("current")) })
		if !ok {
			u.settingsError(w, r, "pin", msg)
			return
		}
	}
	next := strings.TrimSpace(r.PostFormValue("pin"))
	if err := admin.SetPIN(r.Context(), next); err != nil {
		u.settingsError(w, r, "pin", sentence(err.Error()))
		return
	}
	u.log.Info("handoff PIN changed from the UI")
	done(w, r, "/settings", "PIN saved", false)
}

// checkCurrent checks a current PIN or password, a few tries at a time.
func (u *UI) checkCurrent(r *http.Request, check func() (bool, error)) (bool, string) {
	try, _ := u.pinTries.Begin(u.web.ClientIP(r))
	if try == nil {
		return false, "Too many wrong tries. Wait a few minutes."
	}
	ok, err := check()
	switch {
	case err != nil:
		try.Cancel()
		u.log.Error("checking a secret", "err", err)
		return false, "Something went wrong."
	case !ok:
		try.Fail()
		return false, "That isn't right."
	}
	try.Succeed()
	return true, ""
}

func (u *UI) changePassword(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	next := r.PostFormValue("password")
	if next != r.PostFormValue("confirm") {
		u.settingsError(w, r, "password", "The new passwords don't match.")
		return
	}
	try, _ := u.pinTries.Begin(u.web.ClientIP(r))
	if try == nil {
		u.settingsError(w, r, "password", "Too many wrong tries. Wait a few minutes.")
		return
	}
	keep := ""
	if c, err := r.Cookie(auth.CookieName); err == nil {
		keep = c.Value
	}
	err := u.web.Admin().ChangePassword(r.Context(), r.PostFormValue("current"), next, keep)
	switch {
	case errors.Is(err, auth.ErrWrongPassword):
		try.Fail()
		u.settingsError(w, r, "password", "The current password isn't right.")
		return
	case errors.Is(err, auth.ErrEnvManaged):
		try.Cancel()
		u.settingsError(w, r, "password", "The password is set by BROWSERPIER_ADMIN_PASSWORD; change it there.")
		return
	case err != nil:
		try.Cancel()
		u.settingsError(w, r, "password", sentence(err.Error()))
		return
	}
	try.Succeed()
	u.log.Info("admin password changed from the UI")
	done(w, r, "/settings", "Password changed. Other devices are signed out", false)
}

func (u *UI) signOutOthers(w http.ResponseWriter, r *http.Request) {
	c, err := r.Cookie(auth.CookieName)
	if err != nil {
		http.Error(w, "not signed in", http.StatusUnauthorized)
		return
	}
	if err := u.web.Admin().EndOtherSessions(r.Context(), c.Value); err != nil {
		u.log.Error("signing out other devices", "err", err)
		done(w, r, "/settings", "Couldn't sign out the other devices", true)
		return
	}
	u.log.Info("other devices signed out from the UI")
	done(w, r, "/settings", "Other devices signed out", false)
}

// saveAppearance keeps this device's theme. The page has already switched,
// so htmx gets nothing back; a plain form goes back to Settings.
func (u *UI) saveAppearance(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	t, ok := theme.Parse(r.PostFormValue("theme"))
	if !ok {
		http.Error(w, "Choose System, Light or Dark.", http.StatusBadRequest)
		return
	}
	theme.Set(w, t)
	if r.Header.Get("HX-Request") == "true" {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	done(w, r, "/settings", "Appearance saved", false)
}

// saveNotify saves the webhook's URL, its events and the address for
// links. A first URL shows its new secret, once. Neither the URL nor the
// secret is logged; an error never echoes the URL.
func (u *UI) saveNotify(w http.ResponseWriter, r *http.Request) {
	if u.o.Notify == nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	c := notify.Change{URL: strings.TrimSpace(r.PostFormValue("url")), LinkBase: r.PostFormValue("link_base"), On: map[notify.Kind]bool{}}
	for _, k := range r.PostForm["event"] {
		c.On[notify.Kind(k)] = true
	}
	secret, err := u.o.Notify.Save(r.Context(), c)
	if err != nil {
		u.settingsError(w, r, "notify", sentence(err.Error()))
		return
	}
	u.log.Info("notifications saved from the UI", "webhook_set", u.o.Notify.View().Set)
	if secret != "" {
		u.showSecret(w, r, secret)
		return
	}
	done(w, r, "/settings#notify", "Notifications saved", false)
}

// showSecret renders Settings with the webhook's new secret, which is
// never shown again.
func (u *UI) showSecret(w http.ResponseWriter, r *http.Request, secret string) {
	d, err := u.settingsData(r)
	if err != nil {
		u.log.Error("reading settings", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	d.NewSecret = secret
	u.render(w, r, http.StatusOK, "settings", "Settings", "settings", d)
}

func (u *UI) removeNotify(w http.ResponseWriter, r *http.Request) {
	if u.o.Notify == nil {
		http.NotFound(w, r)
		return
	}
	if err := u.o.Notify.Remove(r.Context()); err != nil {
		u.log.Error("removing the webhook", "err", err)
		done(w, r, "/settings#notify", "Couldn't remove the webhook", true)
		return
	}
	u.log.Info("webhook removed from the UI")
	done(w, r, "/settings#notify", "Webhook removed", false)
}

func (u *UI) regenerateNotify(w http.ResponseWriter, r *http.Request) {
	if u.o.Notify == nil {
		http.NotFound(w, r)
		return
	}
	secret, err := u.o.Notify.Regenerate(r.Context())
	if err != nil {
		u.settingsError(w, r, "notify", sentence(err.Error()))
		return
	}
	u.log.Info("webhook secret regenerated from the UI")
	u.showSecret(w, r, secret)
}

// testNotify sends the test event once and says what the webhook
// answered: its status, or a short error. Never the answer's body.
func (u *UI) testNotify(w http.ResponseWriter, r *http.Request) {
	if u.o.Notify == nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), notify.Timeout+5*time.Second)
	defer cancel()
	status, err := u.o.Notify.SendTest(ctx)
	switch {
	case errors.Is(err, notify.ErrNoWebhook):
		done(w, r, "/settings#notify", "No webhook is set. Save a URL first", true)
	case err != nil:
		done(w, r, "/settings#notify", "The test didn't arrive: "+err.Error(), true)
	case status < 200 || status > 299:
		done(w, r, "/settings#notify", fmt.Sprintf("The webhook answered %d %s", status, http.StatusText(status)), true)
	default:
		done(w, r, "/settings#notify", fmt.Sprintf("Test sent: the webhook answered %d", status), false)
	}
}
