package ui

import (
	"context"
	"errors"
	"net/http"
	"net/http/cookiejar"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

const (
	storedPass = "st0red-pw-55"
	storedURL  = "http://pier:" + storedPass + "@proxy.example:8080"
)

func proxyFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	f.m.SetGeoURL("https://geo.example/json")
	return f
}

func (f *fixture) withProxy(name, proxy string) profiles.Profile {
	f.t.Helper()
	p := profiles.New(name)
	p.Proxy = proxy
	p, err := f.m.Profiles().Create(context.Background(), p)
	if err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) tested() []string {
	f.rt.Mu.Lock()
	defer f.rt.Mu.Unlock()
	return append([]string(nil), f.rt.ProxyTests...)
}

func (f *fixture) testProxy(form url.Values, header ...string) reply {
	f.t.Helper()
	return f.post("/proxy/test", form, header...)
}

func TestProxyTestIsAdminOnly(t *testing.T) {
	f := proxyFixture(t)
	f.withProxy("bank", storedURL)
	form := url.Values{"proxy": {"http://u:p@other.example:8080"}, "form": {"settings"}}
	jar := f.client.Jar
	f.client.Jar, _ = cookiejar.New(nil)
	if r := f.testProxy(form); r.code != http.StatusUnauthorized {
		t.Fatalf("signed out: %d", r.code)
	}
	f.client.Jar = jar
	if r := f.testProxy(form, "Sec-Fetch-Site", "cross-site"); r.code != http.StatusForbidden {
		t.Fatalf("cross-site: %d", r.code)
	}
	// An agent's token isn't the admin's session.
	f.client.Jar, _ = cookiejar.New(nil)
	_, tok, err := f.tokens.Create(context.Background(), "bot", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if r := f.testProxy(form, "Authorization", "Bearer "+tok); r.code != http.StatusUnauthorized {
		t.Fatalf("with an agent token: %d", r.code)
	}
	f.client.Jar = jar
	if got := f.tested(); len(got) != 0 {
		t.Fatalf("tested %v", got)
	}
}

func TestProxyTestResolvesTheMask(t *testing.T) {
	f := proxyFixture(t)
	f.withProxy("bank", storedURL)
	f.withProxy("other", "http://pier:0ther-pw@proxy.example:8080")
	masked := maskProxy(storedURL)

	// Edit: the mask stands for the profile's own proxy.
	r := f.testProxy(url.Values{"proxy": {masked}, "profile": {"bank"}, "form": {"edit"}})
	want(t, r, http.StatusOK, "Works.", "198.51.100.23", "Europe/Berlin", "312 ms", "The proxy and this result weren’t saved.")
	// Another profile's mask is its own, never this one's.
	f.testProxy(url.Values{"proxy": {maskProxy("http://pier:0ther-pw@proxy.example:8080")}, "profile": {"other"}, "form": {"edit"}})
	// A new URL is tested as typed.
	f.testProxy(url.Values{"proxy": {"socks5://u:typed-pw@new.example:1080"}, "profile": {"bank"}, "form": {"edit"}})
	got := f.tested()
	if len(got) != 3 || got[0] != storedURL || got[1] != "http://pier:0ther-pw@proxy.example:8080" || got[2] != "socks5://u:typed-pw@new.example:1080" {
		t.Fatalf("tested %v", got)
	}
	if strings.Contains(r.body, storedPass) || strings.Contains(r.body, "proxy.example") {
		t.Fatalf("the answer holds the stored proxy: %s", r.body)
	}

	// Settings and New profile: the mask stands for the default.
	st, _ := f.settings.Get(context.Background())
	st.DefaultProxy = "http://dflt:d3fault-pw@default.example:3128"
	if err := f.settings.Save(context.Background(), st); err != nil {
		t.Fatal(err)
	}
	for _, form := range []string{"settings", "new"} {
		r := f.testProxy(url.Values{"proxy": {maskProxy(st.DefaultProxy)}, "form": {form}})
		want(t, r, http.StatusOK, "Works.")
		if strings.Contains(r.body, "d3fault-pw") {
			t.Fatal("the answer holds the default's password")
		}
	}
	// A profile's mask on Settings stands for nothing of the profile's.
	f.testProxy(url.Values{"proxy": {masked}, "form": {"settings"}})
	got = f.tested()
	if got[3] != st.DefaultProxy || got[4] != st.DefaultProxy {
		t.Fatalf("tested %v", got)
	}
	for _, g := range got[5:] {
		if g == storedURL {
			t.Fatal("Settings tested a profile's proxy")
		}
	}
}

func TestProxyTestRefusesWhatCantBeSaved(t *testing.T) {
	f := proxyFixture(t)
	f.withProxy("bank", storedURL)
	want(t, f.testProxy(url.Values{"proxy": {""}, "form": {"settings"}}), http.StatusUnprocessableEntity, proxyEmpty)
	want(t, f.testProxy(url.Values{"proxy": {"ftp://u:p@x.example:21"}, "form": {"settings"}}), http.StatusUnprocessableEntity,
		"Proxy: the scheme must be http, https or socks5.")
	want(t, f.testProxy(url.Values{"proxy": {"http://u:p@x.example"}, "profile": {"bank"}}), http.StatusUnprocessableEntity, "give a host and a port")
	want(t, f.testProxy(url.Values{"proxy": {"http://u:p@x.example:1"}, "profile": {"gone"}}), http.StatusNotFound, escaped(proxyNotFound))
	if got := f.tested(); len(got) != 0 {
		t.Fatalf("tested %v", got)
	}
}

func TestProxyTestSentences(t *testing.T) {
	f := proxyFixture(t)
	form := url.Values{"proxy": {"http://u:p@x.example:8080"}, "form": {"settings"}}
	for reason, sentence := range proxyProblems {
		f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
			return docker.ProxyResult{Reason: reason}, nil
		}
		f.app.proxyTests = proxyLimiter{}
		want(t, f.testProxy(form), http.StatusOK, escaped(sentence))
	}
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		return docker.ProxyResult{}, errors.New("daemon down")
	}
	want(t, f.testProxy(form), http.StatusOK, escaped(proxyProblems[docker.ProxyFailed]))
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		return docker.ProxyResult{}, docker.ErrNoProxyCheck
	}
	f.app.proxyTests = proxyLimiter{}
	want(t, f.testProxy(form), http.StatusOK, escaped(proxyNoCheck))
	// The lookup off: no test at all.
	f.m.SetGeoURL("")
	n := len(f.tested())
	want(t, f.testProxy(form), http.StatusOK, escaped(proxyProblems[docker.ProxyLookupOff]))
	if len(f.tested()) != n {
		t.Fatal("a test ran with the lookup off")
	}

	// Every value is escaped, though the parser lets no tag through.
	f.m.SetGeoURL("https://geo.example/json")
	f.app.proxyTests = proxyLimiter{}
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		return docker.ProxyResult{OK: true, IP: netip.MustParseAddr("198.51.100.23"), Country: "<b>DE</b>", TimeZone: "<b>Berlin</b>",
			Locale: "<i>de</i>", Accept: []string{"<script>x</script>"}, Lookup: time.Millisecond}, nil
	}
	r := f.testProxy(form)
	if strings.Contains(r.body, "<b>") || strings.Contains(r.body, "<i>") || strings.Contains(r.body, "<script>") || !strings.Contains(r.body, "&lt;b&gt;DE&lt;/b&gt;") {
		t.Fatalf("not escaped: %s", r.body)
	}
}

// escaped is s as html/template writes it.
func escaped(s string) string {
	return strings.NewReplacer("'", "&#39;", `"`, "&#34;").Replace(s)
}

func TestProxyTestComparesWithTheProfile(t *testing.T) {
	f := proxyFixture(t)
	ctx := context.Background()
	p := f.withProxy("bank", storedURL)
	form := url.Values{"proxy": {maskProxy(storedURL)}, "profile": {"bank"}}

	// Never run: nothing to compare.
	r := f.testProxy(form)
	want(t, r, http.StatusOK, "Works.", "This profile&#39;s browser runs in Europe/Berlin, in de-DE (de-DE, de, en-US, en).")
	if strings.Contains(r.body, "Last time") || strings.Contains(r.body, "Same country") || strings.Contains(r.body, "set by hand") {
		t.Fatalf("a profile that never ran compared: %s", r.body)
	}

	// Same country, another zone than the exit's: the start keeps the
	// stored zone, and the main line says so.
	if err := f.m.Profiles().SetLocation(ctx, "bank", profiles.Location{Country: "DE", TimeZone: "Europe/Busingen"}); err != nil {
		t.Fatal(err)
	}
	f.app.proxyTests = proxyLimiter{}
	r = f.testProxy(form)
	want(t, r, http.StatusOK, "This profile&#39;s browser runs in Europe/Busingen, in de-DE",
		"Same country as last time, so it keeps its time zone, Europe/Busingen, rather than the exit&#39;s Europe/Berlin.")
	if strings.Contains(r.body, "runs in Europe/Berlin") {
		t.Fatalf("the main line gives the exit's zone, not the one the browser keeps: %s", r.body)
	}
	// Same zone: just kept.
	if err := f.m.Profiles().SetLocation(ctx, "bank", profiles.Location{Country: "DE", TimeZone: "Europe/Berlin"}); err != nil {
		t.Fatal(err)
	}
	want(t, f.testProxy(form), http.StatusOK, "runs in Europe/Berlin", "Same country as last time, so it keeps its time zone.")

	// Another country: a neutral notice, never the orange one.
	if err := f.m.Profiles().SetLocation(ctx, "bank", profiles.Location{Country: "FR", TimeZone: "Europe/Paris"}); err != nil {
		t.Fatal(err)
	}
	r = f.testProxy(form)
	want(t, r, http.StatusOK, `<p class="notice" style="margin:8px 0 0">Last time this profile ran from FR, not DE. Sites may ask its accounts to sign in or verify again.`,
		"runs in Europe/Berlin, in de-DE")
	if strings.Contains(r.body, "warn") {
		t.Fatal("the country change uses the orange box")
	}

	// Set by hand: those win, in the main line too.
	f.app.proxyTests = proxyLimiter{}
	p, _ = f.m.Profiles().Get(ctx, "bank")
	p.TimeZone, p.Locale = "America/New_York", "fr-CA"
	if _, err := f.m.Profiles().Update(ctx, p); err != nil {
		t.Fatal(err)
	}
	want(t, f.testProxy(form), http.StatusOK, "runs in America/New_York, in fr-CA (fr-CA, fr, en-US, en).",
		"Its time zone is set by hand, and stays.", "Its language is set by hand, and stays.")
	if err := f.m.Profiles().SetLocation(ctx, "bank", profiles.Location{Country: "DE", TimeZone: "Europe/Busingen"}); err != nil {
		t.Fatal(err)
	}
	if r := f.testProxy(form); !strings.Contains(r.body, "runs in America/New_York") || strings.Contains(r.body, "keeps its time zone") {
		t.Fatalf("a zone set by hand beats the stored one: %s", r.body)
	}
}

// TestProxyAnswersAreMarked: app.js shows only answers marked as
// BrowserPier's own fragment.
func TestProxyAnswersAreMarked(t *testing.T) {
	f := proxyFixture(t)
	for _, form := range []url.Values{
		{"proxy": {"http://u:p@x.example:8080"}, "form": {"settings"}},
		{"proxy": {""}, "form": {"settings"}},
	} {
		if r := f.testProxy(form); r.header.Get(proxyFragmentHeader) != "proxy-result" {
			t.Fatalf("%d answer without the mark", r.code)
		}
	}
}

func TestProxyTestLimits(t *testing.T) {
	f := proxyFixture(t)
	now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
	var clock sync.Mutex
	f.app.now = func() time.Time { clock.Lock(); defer clock.Unlock(); return now }
	form := url.Values{"proxy": {"http://u:p@x.example:8080"}, "form": {"settings"}}

	// One at a time.
	running, release := make(chan struct{}), make(chan struct{})
	f.rt.ProxyTest = func(context.Context, string) (docker.ProxyResult, error) {
		close(running)
		<-release
		return docker.ProxyResult{Reason: docker.ProxyRefused}, nil
	}
	first := make(chan reply)
	go func() { first <- f.testProxy(form) }()
	<-running
	want(t, f.testProxy(form), http.StatusTooManyRequests, proxyBusy)
	close(release)
	want(t, <-first, http.StatusOK, escaped(proxyProblems[docker.ProxyRefused]))

	// Six a minute, for the whole instance.
	f.rt.ProxyTest = nil
	for range proxyTestsPerMinute - 1 {
		want(t, f.testProxy(form), http.StatusOK, "Works.")
	}
	want(t, f.testProxy(form), http.StatusTooManyRequests, escaped(proxyTooMany))
	if got := len(f.tested()); got != proxyTestsPerMinute {
		t.Fatalf("%d tests ran", got)
	}
	clock.Lock()
	now = now.Add(time.Minute)
	clock.Unlock()
	want(t, f.testProxy(form), http.StatusOK, "Works.")
}

// TestProxyTestSavesNothing: the profile and the settings are as they
// were, down to updated_at.
func TestProxyTestSavesNothing(t *testing.T) {
	f := proxyFixture(t)
	ctx := context.Background()
	f.withProxy("bank", storedURL)
	if err := f.m.Profiles().SetLocation(ctx, "bank", profiles.Location{Country: "FR", TimeZone: "Europe/Paris"}); err != nil {
		t.Fatal(err)
	}
	st, _ := f.settings.Get(ctx)
	st.DefaultProxy = "http://dflt:d3fault-pw@default.example:3128"
	if err := f.settings.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	before, _ := f.m.Profiles().Get(ctx, "bank")
	beforeSt, _ := f.settings.Get(ctx)
	f.testProxy(url.Values{"proxy": {maskProxy(storedURL)}, "profile": {"bank"}})
	f.testProxy(url.Values{"proxy": {"socks5://u:typed@new.example:1080"}, "profile": {"bank"}})
	f.testProxy(url.Values{"proxy": {maskProxy(st.DefaultProxy)}, "form": {"settings"}})
	f.testProxy(url.Values{"proxy": {"http://u:p@x.example:8080"}, "form": {"new"}})
	after, _ := f.m.Profiles().Get(ctx, "bank")
	afterSt, _ := f.settings.Get(ctx)
	if !reflect.DeepEqual(before, after) {
		t.Fatalf("the profile changed:\n%+v\n%+v", before, after)
	}
	if !reflect.DeepEqual(beforeSt, afterSt) {
		t.Fatalf("the settings changed:\n%+v\n%+v", beforeSt, afterSt)
	}
	if all, _ := f.m.Profiles().List(ctx); len(all) != 1 {
		t.Fatalf("%d profiles: a test on New profile created one", len(all))
	}
}

func TestProxyTestFillsSticky(t *testing.T) {
	f := proxyFixture(t)
	ctx := context.Background()
	tmpl := "http://pier-{sticky}:" + storedPass + "@proxy.example:8080"
	f.withProxy("bank", tmpl)
	f.withProxy("work", tmpl)
	r := f.testProxy(url.Values{"proxy": {maskProxy(tmpl)}, "profile": {"bank"}})
	want(t, r, http.StatusOK, "this profile&#39;s own {sticky} code")
	tok, _ := f.m.Profiles().StickyToken(ctx, "bank")
	work, _ := f.m.Profiles().StickyToken(ctx, "work")
	if got := f.tested()[0]; got != profiles.FillSticky(tmpl, tok) {
		t.Fatalf("Edit tested %q, not the profile's own %s", got, tok)
	}
	if strings.Contains(r.body, tok) {
		t.Fatal("the answer holds the profile's token")
	}

	st, _ := f.settings.Get(ctx)
	st.DefaultProxy = tmpl
	if err := f.settings.Save(ctx, st); err != nil {
		t.Fatal(err)
	}
	for i, form := range []string{"settings", "new"} {
		r := f.testProxy(url.Values{"proxy": {maskProxy(tmpl)}, "form": {form}})
		want(t, r, http.StatusOK, "a sample {sticky} code", "may be in another city or country")
		got := f.tested()[1+i]
		if strings.Contains(got, "{sticky}") || got == profiles.FillSticky(tmpl, tok) || got == profiles.FillSticky(tmpl, work) {
			t.Fatalf("%s tested %q", form, got)
		}
		throwaway := strings.TrimSuffix(strings.TrimPrefix(got, "http://pier-"), ":"+storedPass+"@proxy.example:8080")
		if strings.Contains(r.body, throwaway) {
			t.Fatal("the answer holds the throwaway token")
		}
	}
}

func TestProxyFieldsHaveATestButton(t *testing.T) {
	f := proxyFixture(t)
	f.withProxy("bank", storedURL)
	f.profile("plain")
	want(t, f.get("/profiles/bank/edit"), http.StatusOK, `data-proxy-test="edit" data-profile="bank">Test</button>`)
	want(t, f.get("/profiles/plain/edit"), http.StatusOK, `data-proxy-test="edit" data-profile="plain" disabled>Test</button>`)
	want(t, f.get("/profiles/new"), http.StatusOK, `data-proxy-test="new" disabled>Test</button>`)
	want(t, f.get("/profiles/new", "HX-Request", "true"), http.StatusOK, `<button class="btn" type="button" data-proxy-test="new"`)
	want(t, f.get("/settings"), http.StatusOK, `data-proxy-test="settings" disabled>Test</button>`)
}
