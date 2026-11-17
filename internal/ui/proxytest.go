package ui

import (
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// proxyTestsPerMinute is how many proxy tests the whole instance runs in
// a minute: each starts a container and reaches a host the admin named.
const proxyTestsPerMinute = 6

// proxyLimiter lets one test run at a time, and a few a minute, for the
// whole instance: not per session cookie.
type proxyLimiter struct {
	run sync.Mutex // held by the test running
	mu  sync.Mutex
	at  []time.Time // when the tests of the last minute began
}

// allow reports whether another test may begin now, and counts it if so.
func (l *proxyLimiter) allow(now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	recent := l.at[:0]
	for _, t := range l.at {
		if now.Sub(t) < time.Minute {
			recent = append(recent, t)
		}
	}
	l.at = recent
	if len(l.at) >= proxyTestsPerMinute {
		return false
	}
	l.at = append(l.at, now)
	return true
}

// proxyResultView is a test's answer as the page shows it: BrowserPier's
// own sentences, and values the parser checked.
type proxyResultView struct {
	OK       bool
	Problem  string // why not, when not OK
	IP       string
	Country  string
	TimeZone string
	Locale   string
	Accept   string
	MS       int64
	Warn     string   // the exit's country isn't the profile's last
	Notes    []string // what else a start there would do
}

// The sentence for each way a test can fail. Never the proxy's or the
// endpoint's words.
var proxyProblems = map[string]string{
	docker.ProxyUnreachable: "Couldn't reach the proxy: its name doesn't resolve, or nothing answers at that host and port.",
	docker.ProxyTLS:         "The secure connection to the proxy failed. Check it is an https proxy with a valid certificate.",
	docker.ProxyAuth:        "The proxy refused the user name or password.",
	docker.ProxyRefused:     "The proxy answered, but wouldn't open a connection through it.",
	docker.ProxyTimeout:     "The proxy didn't answer in time.",
	docker.ProxyLookup:      "The proxy opened a connection, but the location lookup gave no usable answer. The proxy may still work; check the lookup endpoint, BROWSERPIER_GEOIP_URL.",
	docker.ProxyLookupOff:   "The location lookup is off (BROWSERPIER_GEOIP_URL=off), so proxies can't be tested.",
	docker.ProxyFailed:      "The test couldn't run. The server's log says why.",
}

const (
	proxyNoCheck  = "This browser image can't test proxies; update it."
	proxyBusy     = "A proxy test is already running."
	proxyTooMany  = "That's 6 tests in a minute. Wait a moment, then test again."
	proxyEmpty    = "Type a proxy URL to test."
	proxyNotFound = "That profile doesn't exist any more."
)

// testProxy tests the proxy typed in a form, on Edit with the profile's
// own {sticky} code, and answers with the result to show under the
// field. It saves nothing. The stored URL, which the field shows masked,
// never goes back to the page.
func (u *UI) testProxy(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	value := strings.TrimSpace(r.PostFormValue("proxy"))
	name := r.PostFormValue("profile")
	t := sessions.ProxyTest{For: "new profile"}
	if r.PostFormValue("form") == "settings" {
		t.For = "settings"
	}
	var p profiles.Profile
	if name != "" {
		var err error
		if p, err = u.m.Profiles().Get(r.Context(), name); err != nil {
			u.proxyAnswer(w, http.StatusNotFound, proxyResultView{Problem: proxyNotFound})
			return
		}
		t.Profile, t.For = p.Name, p.Name
		if value != "" && value == maskProxy(p.Proxy) {
			value = p.Proxy
		}
	} else {
		st, err := u.settings.Get(r.Context())
		if err != nil {
			u.log.Error("reading settings", "err", err)
			u.proxyAnswer(w, http.StatusInternalServerError, proxyResultView{Problem: proxyProblems[docker.ProxyFailed]})
			return
		}
		// New profile shows the default, masked, as Settings does.
		if value != "" && value == maskProxy(st.DefaultProxy) {
			value = st.DefaultProxy
		}
	}
	if value == "" {
		u.proxyAnswer(w, http.StatusUnprocessableEntity, proxyResultView{Problem: proxyEmpty})
		return
	}
	if err := profiles.ValidProxy(value); err != nil {
		u.proxyAnswer(w, http.StatusUnprocessableEntity, proxyResultView{Problem: sentence(err.Error())})
		return
	}
	t.Proxy = value

	if !u.proxyTests.run.TryLock() {
		u.proxyAnswer(w, http.StatusTooManyRequests, proxyResultView{Problem: proxyBusy})
		return
	}
	defer u.proxyTests.run.Unlock()
	if !u.proxyTests.allow(u.now()) {
		u.proxyAnswer(w, http.StatusTooManyRequests, proxyResultView{Problem: proxyTooMany})
		return
	}
	res, err := u.m.TestProxy(r.Context(), t)
	switch {
	case errors.Is(err, sessions.ErrProxyTestBusy):
		u.proxyAnswer(w, http.StatusTooManyRequests, proxyResultView{Problem: proxyBusy})
		return
	case errors.Is(err, docker.ErrNoProxyCheck):
		u.proxyAnswer(w, http.StatusOK, proxyResultView{Problem: proxyNoCheck})
		return
	case err != nil:
		res = docker.ProxyResult{Reason: docker.ProxyFailed}
	}
	u.proxyAnswer(w, http.StatusOK, proxyView(res, p, name != "", profiles.HasSticky(value)))
}

// proxyView words a result, comparing it on Edit with what the profile
// ran with last time and what is set by hand.
func proxyView(res docker.ProxyResult, p profiles.Profile, edit, sticky bool) proxyResultView {
	if !res.OK {
		problem, ok := proxyProblems[res.Reason]
		if !ok {
			problem = proxyProblems[docker.ProxyFailed]
		}
		return proxyResultView{Problem: problem}
	}
	v := proxyResultView{OK: true, IP: res.IP.String(), Country: res.Country, TimeZone: res.TimeZone, Locale: res.Locale,
		Accept: strings.Join(res.Accept, ", "), MS: res.Lookup.Milliseconds()}
	switch {
	case sticky && edit:
		v.Notes = append(v.Notes, "Tested with this profile's own {sticky} code, so this is the exit it gets.")
	case sticky:
		v.Notes = append(v.Notes, "Tested with a sample {sticky} code. Each profile gets its own code, so its exit may be in another city or country.")
	}
	if !edit {
		return v
	}
	switch last := p.Location; {
	case last.Country != "" && last.Country != res.Country:
		v.Warn = "Last time this profile ran from " + last.Country + ", not " + res.Country + ". Sites may ask its accounts to sign in or verify again."
	case last.Country != "" && last.TimeZone != "" && p.TimeZone == "":
		v.Notes = append(v.Notes, "Same country as last time, so the browser keeps its time zone, "+last.TimeZone+".")
	}
	if p.TimeZone != "" {
		v.Notes = append(v.Notes, "This profile's time zone is set by hand, "+p.TimeZone+", and stays.")
	}
	if p.Locale != "" {
		v.Notes = append(v.Notes, "This profile's language is set by hand, "+p.Locale+", and stays.")
	}
	return v
}

func (u *UI) proxyAnswer(w http.ResponseWriter, status int, v proxyResultView) {
	u.fragment(w, status, "settings", "proxy-result", v)
}
