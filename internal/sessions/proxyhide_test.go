package sessions

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// TestProxySecretsForms (PROJ-205): a proxy's parts are hidden in the
// forms a log writes them: user info raw, escaped and decoded, the URL
// with its password masked, a host lowercased, an IPv6 address in its
// canonical form; a bare host name without a dot isn't hidden alone.
func TestProxySecretsForms(t *testing.T) {
	const raw = "http://acct=123%40isp:se;cret%3Apw@Hidden-Proxy.Example:31877"
	u, _ := url.Parse(raw)
	sc := scrub.New(proxySecrets(raw))
	for _, leak := range []string{
		raw, u.Redacted(), "acct=123%40isp", "acct=123@isp", "se;cret%3Apw", "se;cret:pw",
		"Hidden-Proxy.Example", "hidden-proxy.example", "dial tcp: lookup hidden-proxy.example:31877",
		"x509: certificate is valid for a.test, not hidden-proxy.example",
	} {
		if got := sc.String("[" + leak + "]"); strings.Contains(got, "isp") || strings.Contains(got, "cret") || strings.Contains(strings.ToLower(got), "hidden-proxy") {
			t.Errorf("%q came out as %q", leak, got)
		}
	}

	v6 := scrub.New(proxySecrets("socks5://user1:pass1@[2001:DB8:0:0::1]:31877"))
	for _, leak := range []string{"dial tcp [2001:db8::1]:31877: connect: connection refused", "2001:db8::1", "[2001:DB8:0:0::1]:31877"} {
		if got := v6.String(leak); strings.Contains(strings.ToLower(got), "2001:db8") {
			t.Errorf("%q came out as %q", leak, got)
		}
	}

	bare := scrub.New(proxySecrets("http://proxy:3128"))
	if got := bare.String("couldn't reach the proxy"); got != "couldn't reach the proxy" {
		t.Errorf("a dot-less host name hid a word: %q", got)
	}
	if got := bare.String("dial tcp proxy:3128: i/o timeout"); strings.Contains(got, "proxy:3128") {
		t.Errorf("the host with its port shows: %q", got)
	}
}

// TestScrubLogLineKeepsWhatTheLineIs: a credential that is a word of the
// log's own (ERROR, level) doesn't hide the line's level, so a failed
// start's reason is still read from its last log line.
func TestScrubLogLineKeepsWhatTheLineIs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	p, _ := f.m.Profiles().Get(ctx, "google")
	p.Proxy = "http://ERROR:level@hidden-proxy.example:31877"
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	status := 1
	f.rt.ExitOnStart = &status
	f.rt.LastLog = `time=2026-11-27T10:00:00Z level=ERROR msg=exiting err="couldn't reach ERROR:level@hidden-proxy.example:31877"`
	s, _, err := f.m.Start(ctx, "google", "")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("Start = %+v, %v; want it to fail", s, err)
	}
	got := f.stored(s.ID).LastLog
	if strings.Contains(got, "hidden-proxy") || strings.Contains(got, "ERROR:level") {
		t.Fatalf("last log %q", got)
	}
	if why := CrashReason(got); !strings.Contains(why, "couldn't reach") {
		t.Fatalf("the reason is lost: %q from %q", why, got)
	}
}

// leakyProxy is a {sticky} proxy, and a last log line quoting it filled
// in, as an older image's could.
func leakyProxy(t *testing.T, m *Manager) (template string, lastLog func() string) {
	t.Helper()
	ctx := context.Background()
	template = "http://acct-9f3k2-{sticky}:pw-7q2x@hidden-proxy.example:31877"
	p, _ := m.Profiles().Get(ctx, "google")
	p.Proxy = template
	if _, err := m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	return template, func() string {
		tok, err := m.Profiles().StickyToken(ctx, "google")
		if err != nil {
			t.Fatal(err)
		}
		return `level=ERROR msg=exiting err="dial tcp: lookup hidden-proxy.example: no such host; ` +
			profiles.FillSticky(template, tok) + `"`
	}
}

func holdsProxy(text string) bool {
	return strings.Contains(text, "hidden-proxy") || strings.Contains(text, "acct-9f3k2") || strings.Contains(text, "pw-7q2x")
}

// TestReconcileHidesTheProxy (PROJ-205): a browser picked up after a
// restart, here stopped as an older version's, has its proxy hidden from
// its last log line too.
func TestReconcileHidesTheProxy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, lastLog := leakyProxy(t, f.m)
	old := f.start(f.m)
	f.m.Close()
	f.rt.Mu.Lock()
	f.rt.Containers[old.ContainerID].Version = "" // from before the label
	f.rt.LastLog = lastLog()
	f.rt.Mu.Unlock()

	m := f.manager()
	if err := m.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	got := f.stored(old.ID)
	if got.State != Stopped || holdsProxy(got.LastLog) || holdsProxy(got.Error) || !strings.Contains(got.LastLog, scrub.Hidden) {
		t.Fatalf("stopped after the restart: %+v", got)
	}
}

// TestSessionErrorHidesTheProxy: a start failing with an error that
// quotes the proxy keeps it out of the session's error.
func TestSessionErrorHidesTheProxy(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	_, lastLog := leakyProxy(t, f.m)
	f.rt.LastLog = lastLog()
	f.rt.StartErrs = []error{errors.New("can't start: " + lastLog())}
	s, _, err := f.m.Start(ctx, "google", "")
	if !errors.Is(err, ErrStartFailed) {
		t.Fatalf("Start = %+v, %v; want it to fail", s, err)
	}
	stored := f.stored(s.ID)
	for _, text := range []string{s.Error, s.LastLog, stored.Error, stored.LastLog} {
		if holdsProxy(text) {
			t.Errorf("the proxy shows in %q", text)
		}
	}
	if !strings.Contains(stored.Error, scrub.Hidden) {
		t.Errorf("error %q", stored.Error)
	}
}
