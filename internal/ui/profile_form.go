package ui

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// screens are the sizes offered; a profile with another keeps its own.
var screens = []string{"1280x800", "1440x900", "1920x1080"}

type profileForm struct {
	New     bool
	Modal   bool // rendered inside the dialog on Profiles
	Profile profiles.Profile
	Proxy   string // as shown: the password masked
	Start   bool
	Screens []string
	Error   string
	Running bool
	// IconURL is the profile's favicon when editing one that has it.
	IconURL string
}

func (f profileForm) Summary() string {
	proxy := "no proxy"
	if f.Profile.Proxy != "" {
		proxy = "proxy"
	}
	s := fmt.Sprintf("%s · auto-stop %s · %d × %d · %s", modeLabel(f.Profile.DefaultMode),
		strings.TrimSuffix(autoStopLabel(f.Profile.AutoStop), " idle"), f.Profile.Width, f.Profile.Height, proxy)
	if f.Profile.LocalNetwork {
		s += " · local network"
	}
	return s
}

// Policies are the handoff policies, least strict first.
func (profileForm) Policies() []policyChoice { return policyChoices }

// AutoStopChoices are the idle timeouts offered, in minutes; 0 is never.
func (f profileForm) AutoStopChoices() []int { return profiles.AutoStopChoices }

// Glyphs and Colors are the fallback icon's choices; "" in the form picks
// one from the name (.Profile.Glyph and .Profile.Color resolve it).
func (profileForm) Glyphs() []string { return profiles.Glyphs }
func (profileForm) Colors() []string { return profiles.Colors }

func (u *UI) newProfile(w http.ResponseWriter, r *http.Request) {
	defaults, err := u.settings.Get(r.Context())
	if err != nil {
		u.log.Error("reading settings", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	f := profileForm{New: true, Profile: defaults.NewProfile(""), Start: true, Screens: screens}
	f.Proxy = maskProxy(f.Profile.Proxy)
	if r.Header.Get("HX-Request") == "true" {
		f.Modal = true
		u.fragment(w, http.StatusOK, "profile-form", "form", f)
		return
	}
	u.render(w, r, http.StatusOK, "profile-form", "New profile", "profiles", f)
}

func (u *UI) createProfile(w http.ResponseWriter, r *http.Request) {
	defaults, err := u.settings.Get(r.Context())
	if err != nil {
		u.log.Error("reading settings", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	p := defaults.NewProfile(strings.TrimSpace(r.PostFormValue("name")))
	f := profileForm{New: true, Modal: r.Header.Get("HX-Request") == "true", Start: r.PostFormValue("start") == "1", Screens: screens}
	f.Error = readProfileForm(r, &p, defaults.DefaultProxy)
	f.Profile, f.Proxy = p, r.PostFormValue("proxy")
	if f.Error == "" {
		if err := p.Validate(); err != nil {
			f.Error = sentence(err.Error())
		}
	}
	if f.Error == "" {
		_, err := u.m.Profiles().Create(r.Context(), p)
		switch {
		case errors.Is(err, profiles.ErrExists):
			f.Error = "There's already a profile called " + p.Name + "."
		case err != nil:
			u.log.Error("creating a profile", "err", err)
			f.Error = "Couldn't create the profile."
		}
	}
	if f.Error != "" {
		u.formError(w, r, f, "New profile")
		return
	}
	u.log.Info("profile created from the UI", "profile", p.Name)
	if !f.Start {
		done(w, r, "/profiles", p.Name+" created", false)
		return
	}
	// The live view starts the browser and opens the stream.
	done(w, r, "/profiles/"+url.PathEscape(p.Name)+"?start=1", "", false)
}

func (u *UI) editProfile(w http.ResponseWriter, r *http.Request) {
	p, err := u.m.Profiles().Get(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, running := u.m.Live(p.Name)
	f := profileForm{Profile: p, Proxy: maskProxy(p.Proxy), Screens: withScreen(p), Running: running, IconURL: u.IconURL(p.Name)}
	u.render(w, r, http.StatusOK, "profile-form", "Edit "+p.Name, "profiles", f)
}

func (u *UI) saveProfile(w http.ResponseWriter, r *http.Request) {
	p, err := u.m.Profiles().Get(r.Context(), r.PathValue("name"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	_, running := u.m.Live(p.Name)
	f := profileForm{Screens: withScreen(p), Running: running, IconURL: u.IconURL(p.Name)}
	oldSite := p.Site
	f.Error = readProfileForm(r, &p, p.Proxy)
	p.TimeZone = strings.TrimSpace(r.PostFormValue("time_zone"))
	p.Locale = strings.TrimSpace(r.PostFormValue("locale"))
	f.Profile, f.Proxy = p, r.PostFormValue("proxy")
	if f.Error == "" {
		if err := p.Validate(); err != nil {
			f.Error = sentence(err.Error())
		}
	}
	if f.Error == "" {
		if _, err := u.m.Profiles().Update(r.Context(), p); err != nil {
			u.log.Error("saving a profile", "profile", p.Name, "err", err)
			f.Error = "Couldn't save the profile."
		}
	}
	if f.Error != "" {
		u.formError(w, r, f, "Edit "+p.Name)
		return
	}
	u.log.Info("profile settings changed from the UI", "profile", p.Name)
	// Asked again now that the site is saved: a browser stopping meanwhile
	// reads its icon for the site saved.
	if _, live := u.m.Live(p.Name); p.Site != oldSite && !live {
		// The icon follows the new site now, not at the next stop. A
		// browser starting meanwhile refreshes it when it stops.
		if err := u.m.RefreshIcon(p.Name); err != nil {
			u.log.Debug("refreshing the profile's icon", "profile", p.Name, "err", err)
		}
	}
	msg := p.Name + " saved"
	if running {
		msg += ". Changes apply the next time it starts"
	}
	done(w, r, "/profiles", msg, false)
}

func (u *UI) formError(w http.ResponseWriter, r *http.Request, f profileForm, title string) {
	if f.Modal {
		u.fragment(w, http.StatusUnprocessableEntity, "profile-form", "form", f)
		return
	}
	u.render(w, r, http.StatusUnprocessableEntity, "profile-form", title, "profiles", f)
}

// readProfileForm reads the settings both forms share into p. keepProxy is
// the proxy to keep when the field still shows it masked.
func readProfileForm(r *http.Request, p *profiles.Profile, keepProxy string) string {
	if v := r.PostFormValue("policy"); v != "" {
		p.HandoffPolicy = profiles.HandoffPolicy(v)
	}
	if v := r.PostFormValue("mode"); v != "" {
		p.DefaultMode = profiles.Mode(v)
	}
	if v := r.PostFormValue("auto_stop"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return "Choose an auto-stop time."
		}
		p.AutoStop = time.Duration(n) * time.Minute
	}
	if v := r.PostFormValue("screen"); v != "" {
		wd, ht, ok := strings.Cut(v, "x")
		width, err1 := strconv.Atoi(wd)
		height, err2 := strconv.Atoi(ht)
		if !ok || err1 != nil || err2 != nil {
			return "Choose a screen size."
		}
		p.Width, p.Height = width, height
	}
	proxy := strings.TrimSpace(r.PostFormValue("proxy"))
	if proxy != "" && proxy == maskProxy(keepProxy) {
		proxy = keepProxy
	}
	p.Proxy = proxy
	p.LocalNetwork = r.PostFormValue("local_network") == "1"
	// A form without the icon fields leaves them as they are.
	if _, ok := r.PostForm["site"]; ok {
		p.Site = profiles.NormalizeSite(r.PostFormValue("site"))
	}
	if _, ok := r.PostForm["icon_glyph"]; ok {
		p.IconGlyph = r.PostFormValue("icon_glyph")
	}
	if _, ok := r.PostForm["icon_color"]; ok {
		p.IconColor = r.PostFormValue("icon_color")
	}
	return ""
}

// maskProxy shows a proxy URL with its password hidden. Submitting it
// unchanged keeps the stored one.
func maskProxy(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "(hidden)"
	}
	userinfo := ""
	if u.User != nil {
		userinfo = u.User.Username()
		if _, ok := u.User.Password(); ok {
			userinfo += ":••••"
		}
		userinfo += "@"
	}
	return u.Scheme + "://" + userinfo + u.Host
}

// withScreen offers the profile's own screen size too, if it isn't one of ours.
func withScreen(p profiles.Profile) []string {
	own := p.Screen()
	for _, s := range screens {
		if s == own {
			return screens
		}
	}
	return append([]string{own}, screens...)
}

// sentence makes a validation error read as a sentence.
func sentence(s string) string {
	if s == "" {
		return s
	}
	s = strings.ToUpper(s[:1]) + s[1:]
	if !strings.HasSuffix(s, ".") {
		s += "."
	}
	return s
}
