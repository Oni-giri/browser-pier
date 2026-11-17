package notify

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/db"
)

const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// verify is deploy/README.md's receiver check, word for word
// (TestTheDocsShowThisCheck keeps them the same).
func verify(secret, header string, body []byte, now time.Time) bool {
	var ts, sig string
	for _, part := range strings.Split(header, ",") {
		k, v, _ := strings.Cut(part, "=")
		switch k {
		case "t":
			ts = v
		case "v1":
			sig = v
		}
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || now.Sub(time.Unix(t, 0)).Abs() > 5*time.Minute {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(ts + "."))
	mac.Write(body)
	want := hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sig), []byte(want))
}

func TestSignature(t *testing.T) {
	body := []byte(`{"event":"test"}`)
	got := Sign(testSecret, 1700000000, body)
	const want = "t=1700000000,v1=22751b8ccca2474a38721b544790f9a62e2ae990c0526953c05926e5d74c4899"
	if got != want {
		t.Fatalf("Sign = %s, want %s", got, want)
	}
	at := time.Unix(1700000000, 0)
	if !verify(testSecret, got, body, at.Add(4*time.Minute)) {
		t.Error("the docs' check refuses a good signature")
	}
	if verify(testSecret, got, []byte(`{"event":"test2"}`), at) {
		t.Error("the docs' check accepts another body")
	}
	if verify(testSecret, strings.Replace(got, "t=1700000000", "t=1700000001", 1), body, at) {
		t.Error("the docs' check accepts another time")
	}
	if verify(testSecret, got, body, at.Add(6*time.Minute)) {
		t.Error("the docs' check accepts a signature 6 minutes old")
	}
	if verify(strings.Repeat("f", 64), got, body, at) {
		t.Error("the docs' check accepts another secret")
	}
}

// TestTheDocsShowThisCheck: deploy/README.md shows verify and the test
// vector as they are here.
func TestTheDocsShowThisCheck(t *testing.T) {
	doc, err := os.ReadFile("../../deploy/README.md")
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile("notify_test.go")
	if err != nil {
		t.Fatal(err)
	}
	start := bytes.Index(src, []byte("func verify("))
	end := bytes.Index(src[start:], []byte("\n}\n"))
	fn := string(src[start : start+end+2])
	if !strings.Contains(string(doc), fn) {
		t.Errorf("deploy/README.md doesn't show the receiver's check as notify_test.go has it:\n%s", fn)
	}
	for _, s := range []string{testSecret, "1700000000", "22751b8ccca2474a38721b544790f9a62e2ae990c0526953c05926e5d74c4899"} {
		if !strings.Contains(string(doc), s) {
			t.Errorf("deploy/README.md doesn't show the test vector's %s", s)
		}
	}
}

// stubLookup answers from a table, and refuses any other name.
func stubLookup(table map[string]string) Lookup {
	return func(_ context.Context, host string) ([]netip.Addr, error) {
		v, ok := table[host]
		if !ok {
			return nil, errors.New("no such host in the stub")
		}
		var out []netip.Addr
		for _, s := range strings.Split(v, ",") {
			out = append(out, netip.MustParseAddr(s))
		}
		return out, nil
	}
}

var publicHosts = map[string]string{"hooks.example.com": "93.184.215.14", "example.com": "93.184.215.14", "x": "93.184.215.14"}

func TestURLRules(t *testing.T) {
	ctx := context.Background()
	lookup := stubLookup(publicHosts)
	for _, c := range []struct {
		url string
		ok  bool
	}{
		{"https://hooks.example.com/a", true},
		{"http://192.168.1.5:8123/x", true},
		{"http://127.0.0.1:9/x", true},
		{"http://100.100.1.1/x", true},
		{"http://[fd00::1]/x", true},
		{"http://hooks.example.com/", false},
		{"https://user:pw@example.com/", false},
		{"https://user@example.com/", false},
		{"ftp://x/", false},
		{"https:///path", false},
		{"https:", false},
		{"https://hooks.example.com/" + strings.Repeat("a", 2049-len("https://hooks.example.com/")), false},
		{"https://169.254.169.254/", false},
		{"http://169.254.169.254/", false},
		{"http://[fe80::1]/", false},
		{"https://[fe80::1]/", false},
		{"https://0.0.0.0/", false},
		{"https://224.0.0.1/", false},
		{"https://255.255.255.255/", false},
		{"https://[::ffff:169.254.169.254]/", false},
	} {
		err := checkURL(ctx, lookup, c.url)
		if (err == nil) != c.ok {
			t.Errorf("checkURL(%.60q) = %v, want ok %v", c.url, err, c.ok)
		}
		if err != nil && strings.Contains(err.Error(), "/a") {
			t.Errorf("checkURL's error echoes the URL: %v", err)
		}
	}
	if err := checkURL(ctx, lookup, "https://hooks.example.com/"+strings.Repeat("a", 2048-len("https://hooks.example.com/"))); err != nil {
		t.Errorf("a URL of 2,048 bytes is refused: %v", err)
	}
	// A name with one bad address among good ones is refused.
	mixed := stubLookup(map[string]string{"hooks.example.com": "93.184.215.14,169.254.169.254"})
	if err := checkURL(ctx, mixed, "https://hooks.example.com/a"); err == nil {
		t.Error("a name with a link-local address among its addresses is accepted")
	}
}

// logBuffer collects everything logged, to check it never holds a secret.
type logBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

type fixture struct {
	t    *testing.T
	n    *Notifier
	logs *logBuffer
	// forbidden are strings the log must never hold.
	forbidden []string
	clock     atomic.Int64 // unix seconds; each now() moves it on by one
}

func newNotifier(t *testing.T, o Options) *fixture {
	t.Helper()
	dir := t.TempDir()
	d, err := db.Open(context.Background(), filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	f := &fixture{t: t, logs: &logBuffer{}}
	if o.Lookup == nil {
		o.Lookup = stubLookup(publicHosts)
	}
	n, err := New(context.Background(), d, slog.New(slog.NewTextHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug})), o)
	if err != nil {
		t.Fatal(err)
	}
	f.clock.Store(1700000000)
	n.now = func() time.Time { return time.Unix(f.clock.Add(1), 0) }
	n.timeout = 2 * time.Second
	n.retries = []time.Duration{10 * time.Millisecond, 20 * time.Millisecond}
	f.n = n
	t.Cleanup(func() {
		got := f.logs.String()
		for _, s := range f.forbidden {
			if s != "" && strings.Contains(got, s) {
				t.Errorf("the log holds %q:\n%s", s, got)
			}
		}
		if strings.Contains(got, "BrowserPier-Signature") || strings.Contains(got, "v1=") || strings.Contains(got, `"text"`) {
			t.Errorf("the log holds a signature or a body:\n%s", got)
		}
	})
	return f
}

// run starts the workers until the test ends.
func (f *fixture) run() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { f.n.Run(ctx); close(done) }()
	f.t.Cleanup(func() { cancel(); <-done })
}

// set saves a webhook URL and returns its secret.
func (f *fixture) set(raw string, on ...Kind) string {
	f.t.Helper()
	toggles := map[Kind]bool{HandoffRequested: true, HandoffExpired: true, HandoffCancelled: true, BrowserFailed: true}
	if len(on) > 0 {
		toggles = map[Kind]bool{}
		for _, k := range on {
			toggles[k] = true
		}
	}
	secret, err := f.n.Save(context.Background(), Change{URL: raw, On: toggles})
	if err != nil {
		f.t.Fatal(err)
	}
	f.forbidden = append(f.forbidden, secret, strings.TrimPrefix(raw, "http://"+hostOf(raw)))
	return secret
}

// receiver is a webhook that answers each POST with the next status in
// turn (the last one after that) and records what it got.
type receiver struct {
	*httptest.Server
	mu       sync.Mutex
	statuses []int
	got      []received
	hook     func(n int, w http.ResponseWriter, r *http.Request) bool // true: answered
	arrived  chan struct{}
}

type received struct {
	body   []byte
	header http.Header
	at     time.Time
}

func newReceiver(t *testing.T, statuses ...int) *receiver {
	rc := &receiver{statuses: statuses, arrived: make(chan struct{}, 256)}
	rc.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rc.mu.Lock()
		rc.got = append(rc.got, received{body: b, header: r.Header.Clone(), at: time.Now()})
		n := len(rc.got)
		status := http.StatusOK
		if len(rc.statuses) > 0 {
			status = rc.statuses[min(n-1, len(rc.statuses)-1)]
		}
		hook := rc.hook
		rc.mu.Unlock()
		rc.arrived <- struct{}{}
		if hook != nil && hook(n, w, r) {
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(rc.Close)
	return rc
}

func (rc *receiver) posts() []received {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return append([]received(nil), rc.got...)
}

// settle waits for n POSTs, then a little more to see no other comes.
func (rc *receiver) settle(t *testing.T, n int) []received {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for range n {
		select {
		case <-rc.arrived:
		case <-deadline:
			t.Fatalf("got %d POSTs, want %d", len(rc.posts()), n)
		}
	}
	time.Sleep(200 * time.Millisecond)
	got := rc.posts()
	if len(got) != n {
		t.Fatalf("got %d POSTs, want %d", len(got), n)
	}
	return got
}

var testHandoff = Handoff{ID: "h_abc", Profile: "google", Reason: "sensitive_input", ReasonLabel: "Sign-in or payment",
	Policy: "pin", Agent: "research-bot", Origin: "https://accounts.google.com", ExpiresAt: time.Unix(1700000900, 0)}

func TestDelivery(t *testing.T) {
	for _, c := range []struct {
		name     string
		statuses []int
		want     int
	}{
		{"200 delivers once", []int{200}, 1},
		{"500 500 200 makes three", []int{500, 500, 200}, 3},
		{"three 500s stop at three", []int{500, 500, 500, 500}, 3},
		{"404 is final", []int{404, 200}, 1},
		{"429 is final", []int{429, 200}, 1},
		{"204 delivers", []int{204, 200}, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := newNotifier(t, Options{Version: "1.2.3"})
			rc := newReceiver(t, c.statuses...)
			secret := f.set(rc.URL + "/hook/secret-path")
			f.run()
			if !f.n.HandoffExpired(testHandoff) {
				t.Fatal("not queued")
			}
			got := rc.settle(t, c.want)
			var id string
			ts := map[string]bool{}
			for i, p := range got {
				var body map[string]any
				if err := json.Unmarshal(p.body, &body); err != nil {
					t.Fatal(err)
				}
				if i == 0 {
					id = body["id"].(string)
				} else if body["id"] != id {
					t.Errorf("try %d has id %v, want %s", i+1, body["id"], id)
				}
				sig := p.header.Get(SignatureHeader)
				tsv, _, _ := strings.Cut(strings.TrimPrefix(sig, "t="), ",")
				n, _ := strconv.ParseInt(tsv, 10, 64)
				if !verify(secret, sig, p.body, time.Unix(n, 0)) {
					t.Errorf("try %d: the signature doesn't verify", i+1)
				}
				if ts[tsv] {
					t.Errorf("try %d reuses t=%s", i+1, tsv)
				}
				ts[tsv] = true
				if p.header.Get("Content-Type") != "application/json" || p.header.Get("User-Agent") != "BrowserPier/1.2.3" {
					t.Errorf("headers: %v", p.header)
				}
			}
		})
	}
}

func TestRedirectsAreNotFollowed(t *testing.T) {
	f := newNotifier(t, Options{})
	target := newReceiver(t)
	rc := newReceiver(t)
	rc.hook = func(_ int, w http.ResponseWriter, r *http.Request) bool {
		http.Redirect(w, r, target.URL+"/elsewhere", http.StatusFound)
		return true
	}
	f.set(rc.URL + "/x")
	f.run()
	f.n.HandoffExpired(testHandoff)
	rc.settle(t, 1)
	if n := len(target.posts()); n != 0 {
		t.Errorf("the redirect's target got %d requests", n)
	}
}

func TestAHangingWebhookIsCutAndRetried(t *testing.T) {
	f := newNotifier(t, Options{})
	rc := newReceiver(t)
	rc.hook = func(_ int, _ http.ResponseWriter, r *http.Request) bool {
		<-r.Context().Done()
		return true
	}
	f.set(rc.URL + "/x")
	f.n.timeout = 100 * time.Millisecond
	f.run()
	start := time.Now()
	f.n.HandoffExpired(testHandoff)
	rc.settle(t, 3)
	if !strings.Contains(f.logs.String(), "timed out") {
		t.Errorf("the log doesn't say it timed out:\n%s", f.logs.String())
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestAChangedWebhookGetsNoRetry(t *testing.T) {
	for _, change := range []string{"replaced", "removed", "secret"} {
		t.Run(change, func(t *testing.T) {
			f := newNotifier(t, Options{})
			other := newReceiver(t)
			rc := newReceiver(t, 500)
			rc.hook = func(n int, _ http.ResponseWriter, _ *http.Request) bool {
				if n == 1 {
					var err error
					switch change {
					case "replaced":
						_, err = f.n.Save(context.Background(), Change{URL: other.URL + "/new", On: map[Kind]bool{HandoffExpired: true}})
					case "removed":
						err = f.n.Remove(context.Background())
					case "secret":
						_, err = f.n.Regenerate(context.Background())
					}
					if err != nil {
						t.Error(err)
					}
				}
				return false
			}
			f.set(rc.URL + "/x")
			f.run()
			f.n.HandoffExpired(testHandoff)
			rc.settle(t, 1)
			if n := len(other.posts()); n != 0 {
				t.Errorf("the new webhook got %d POSTs", n)
			}
		})
	}
}

func TestAnEndedHandoffGetsNoRetry(t *testing.T) {
	f := newNotifier(t, Options{PublicURL: "https://pier.example.com"})
	rc := newReceiver(t, 500)
	f.set(rc.URL + "/x")
	f.run()
	var open atomic.Bool
	open.Store(true)
	rc.hook = func(int, http.ResponseWriter, *http.Request) bool { open.Store(false); return false }
	f.n.HandoffRequested(testHandoff, "tok", open.Load)
	rc.settle(t, 1)
	if !strings.Contains(f.logs.String(), "the handoff has ended") {
		t.Errorf("the log doesn't say why:\n%s", f.logs.String())
	}
}

func TestAFullQueueDrops(t *testing.T) {
	f := newNotifier(t, Options{})
	release := make(chan struct{})
	rc := newReceiver(t)
	rc.hook = func(int, http.ResponseWriter, *http.Request) bool { <-release; return false }
	f.set(rc.URL + "/x")
	f.n.timeout = time.Minute
	f.run()
	defer close(release)
	for range Workers {
		if !f.n.HandoffExpired(testHandoff) {
			t.Fatal("not queued")
		}
	}
	for range Workers {
		select {
		case <-rc.arrived:
		case <-time.After(10 * time.Second):
			t.Fatal("the first POSTs didn't arrive")
		}
	}
	for i := range QueueSize {
		if !f.n.HandoffExpired(testHandoff) {
			t.Fatalf("event %d not queued", i+1)
		}
	}
	start := time.Now()
	if f.n.HandoffExpired(testHandoff) {
		t.Error("an event past 4 under way and 64 waiting is queued")
	}
	if d := time.Since(start); d > 100*time.Millisecond {
		t.Errorf("the caller waited %s", d)
	}
	if !strings.Contains(f.logs.String(), "the queue is full") {
		t.Errorf("the drop isn't logged:\n%s", f.logs.String())
	}
}

func TestFloodCap(t *testing.T) {
	f := newNotifier(t, Options{})
	f.set("https://hooks.example.com/x")
	var now atomic.Int64
	now.Store(1700000000)
	f.n.now = func() time.Time { return time.Unix(now.Load(), 0) }
	for i := range FloodLimit {
		if !f.n.HandoffRequested(testHandoff, "", nil) {
			t.Fatalf("handoff %d not queued", i+1)
		}
		now.Add(60)
	}
	if f.n.HandoffRequested(testHandoff, "", nil) {
		t.Error("the 7th handoff.requested in 15 minutes is queued")
	}
	if !strings.Contains(f.logs.String(), "too many handoffs") {
		t.Errorf("the drop isn't logged:\n%s", f.logs.String())
	}
	other := testHandoff
	other.Profile = "bank"
	if !f.n.HandoffRequested(other, "", nil) {
		t.Error("another profile's handoff is dropped")
	}
	if !f.n.HandoffExpired(testHandoff) {
		t.Error("the cap holds back other events")
	}
	now.Add(int64(FloodWindow / time.Second))
	if !f.n.HandoffRequested(testHandoff, "", nil) {
		t.Error("still dropped 15 minutes later")
	}
}

// TestBodies: each event's body, field by field.
func TestBodies(t *testing.T) {
	var got []string
	f := newNotifier(t, Options{PublicURL: "https://pier.example.com/", OnQueue: func(_ Kind, b []byte) { got = append(got, string(b)) }})
	f.set("https://hooks.example.com/x")
	f.n.newID = func() string { return "0f0f" }
	f.n.now = func() time.Time { return time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC) }
	f.n.HandoffRequested(testHandoff, "tok3n", nil)
	link := testHandoff
	link.Policy = "link"
	f.n.HandoffRequested(link, "", nil)
	f.n.HandoffExpired(testHandoff)
	f.n.HandoffCancelled(testHandoff)
	f.n.BrowserFailed("google", "crashed")
	want := []string{
		`{"id":"0f0f","event":"handoff.requested","time":"2026-11-16T09:30:00Z","profile":"google","handoff_id":"h_abc","reason":"sensitive_input","policy":"pin","agent":"research-bot","origin":"https://accounts.google.com","expires_at":"2023-11-14T22:28:20Z","url":"https://pier.example.com/h/tok3n","text":"BrowserPier · google: an agent needs you (Sign-in or payment). https://pier.example.com/h/tok3n","content":"BrowserPier · google: an agent needs you (Sign-in or payment). https://pier.example.com/h/tok3n"}`,
		`{"id":"0f0f","event":"handoff.requested","time":"2026-11-16T09:30:00Z","profile":"google","handoff_id":"h_abc","reason":"sensitive_input","policy":"link","agent":"research-bot","origin":"https://accounts.google.com","expires_at":"2023-11-14T22:28:20Z","url":"https://pier.example.com/profiles/google","text":"BrowserPier · google: an agent needs you (Sign-in or payment). https://pier.example.com/profiles/google","content":"BrowserPier · google: an agent needs you (Sign-in or payment). https://pier.example.com/profiles/google"}`,
		`{"id":"0f0f","event":"handoff.expired","time":"2026-11-16T09:30:00Z","profile":"google","handoff_id":"h_abc","reason":"sensitive_input","policy":"pin","agent":"research-bot","origin":"https://accounts.google.com","expires_at":"2023-11-14T22:28:20Z","url":"https://pier.example.com/profiles/google","text":"BrowserPier · google: nobody took the handoff within 15 minutes, so it expired. https://pier.example.com/profiles/google","content":"BrowserPier · google: nobody took the handoff within 15 minutes, so it expired. https://pier.example.com/profiles/google"}`,
		`{"id":"0f0f","event":"handoff.cancelled","time":"2026-11-16T09:30:00Z","profile":"google","handoff_id":"h_abc","reason":"sensitive_input","policy":"pin","agent":"research-bot","origin":"https://accounts.google.com","expires_at":"2023-11-14T22:28:20Z","url":"https://pier.example.com/profiles/google","cause":"wrong_pins","text":"BrowserPier · google: five wrong PINs cancelled a handoff. https://pier.example.com/profiles/google","content":"BrowserPier · google: five wrong PINs cancelled a handoff. https://pier.example.com/profiles/google"}`,
		`{"id":"0f0f","event":"browser.failed","time":"2026-11-16T09:30:00Z","profile":"google","url":"https://pier.example.com/profiles/google","category":"crashed","text":"BrowserPier · google: the browser stopped unexpectedly. https://pier.example.com/profiles/google","content":"BrowserPier · google: the browser stopped unexpectedly. https://pier.example.com/profiles/google"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("queued %d events, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("body %d:\n got %s\nwant %s", i, got[i], want[i])
		}
	}
	// An unknown category is no category at all.
	if f.n.BrowserFailed("google", "level=ERROR msg=exiting") {
		t.Error("a made-up category is queued")
	}
}

func TestNoAddressForLinks(t *testing.T) {
	var got []string
	f := newNotifier(t, Options{OnQueue: func(_ Kind, b []byte) { got = append(got, string(b)) }})
	f.set("https://hooks.example.com/x")
	f.n.HandoffRequested(testHandoff, "tok3n", nil)
	f.n.BrowserFailed("google", "lost")
	for _, b := range got {
		if strings.Contains(b, `"url"`) || strings.Contains(b, "tok3n") {
			t.Errorf("with no address for links, a body holds a link: %s", b)
		}
		if !strings.Contains(b, "Open BrowserPier to see it.") {
			t.Errorf("the sentence doesn't say to open BrowserPier: %s", b)
		}
	}
	// The admin's address for links is used when no public URL is set.
	got = nil
	if _, err := f.n.Save(context.Background(), Change{URL: Mask("https://hooks.example.com/x"), On: map[Kind]bool{HandoffRequested: true},
		LinkBase: "https://pier.lan:7437/"}); err != nil {
		t.Fatal(err)
	}
	f.n.HandoffRequested(Handoff{ID: "h_2", Profile: "bank", Policy: "signin"}, "tok", nil)
	if len(got) != 1 || !strings.Contains(got[0], `"url":"https://pier.lan:7437/h/tok"`) {
		t.Errorf("the address for links isn't used: %v", got)
	}
}

func TestNothingIsQueuedWithNoWebhookOrTheEventOff(t *testing.T) {
	queued := 0
	f := newNotifier(t, Options{OnQueue: func(Kind, []byte) { queued++ }})
	if f.n.HandoffRequested(testHandoff, "t", nil) || f.n.HandoffExpired(testHandoff) || f.n.BrowserFailed("google", "crashed") {
		t.Error("queued with no webhook")
	}
	f.set("https://hooks.example.com/x", HandoffExpired)
	if f.n.HandoffRequested(testHandoff, "t", nil) || f.n.BrowserFailed("google", "crashed") || f.n.HandoffCancelled(testHandoff) {
		t.Error("an event turned off is queued")
	}
	if !f.n.HandoffExpired(testHandoff) {
		t.Error("an event turned on isn't queued")
	}
	if queued != 1 {
		t.Errorf("OnQueue saw %d events", queued)
	}
	var nilN *Notifier
	if nilN.HandoffRequested(testHandoff, "t", nil) || nilN.BrowserFailed("google", "crashed") {
		t.Error("a nil notifier queues")
	}
}

func TestBrowserFailedIsOffByDefault(t *testing.T) {
	f := newNotifier(t, Options{})
	if _, err := f.n.Save(context.Background(), Change{URL: "https://hooks.example.com/x", On: defaultOn}); err != nil {
		t.Fatal(err)
	}
	v := f.n.View()
	if v.On[BrowserFailed] || !v.On[HandoffRequested] || !v.On[HandoffExpired] || !v.On[HandoffCancelled] {
		t.Errorf("defaults: %v", v.On)
	}
	// What New reads before any save.
	c, err := load(context.Background(), f.n.db)
	if err != nil {
		t.Fatal(err)
	}
	if c.On[BrowserFailed] || !c.On[HandoffRequested] {
		t.Errorf("stored: %v", c.On)
	}
}

func TestTheSecret(t *testing.T) {
	f := newNotifier(t, Options{})
	ctx := context.Background()
	first, err := f.n.Save(ctx, Change{URL: "https://hooks.example.com/a/one"})
	if err != nil || len(first) != 64 {
		t.Fatalf("first save: %q, %v", first, err)
	}
	if _, err := hex.DecodeString(first); err != nil {
		t.Errorf("the secret isn't hex: %v", err)
	}
	again, err := f.n.Save(ctx, Change{URL: "https://hooks.example.com/a/two"})
	if err != nil || again != "" {
		t.Errorf("changing the URL gives a secret %q, %v", again, err)
	}
	if cfg, _ := f.n.current(); cfg.Secret != first || cfg.URL != "https://hooks.example.com/a/two" {
		t.Errorf("after changing the URL: secret kept %v, url %v", cfg.Secret == first, cfg.URL == "https://hooks.example.com/a/two")
	}
	// The masked URL keeps the saved one.
	if _, err := f.n.Save(ctx, Change{URL: "https://hooks.example.com/…"}); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := f.n.current(); cfg.URL != "https://hooks.example.com/a/two" {
		t.Error("saving the masked URL changed it")
	}
	// An edited mask isn't a URL.
	if _, err := f.n.Save(ctx, Change{URL: "https://other.example.com/…"}); err == nil {
		t.Error("an edited mask was saved")
	}
	next, err := f.n.Regenerate(ctx)
	if err != nil || len(next) != 64 || next == first {
		t.Fatalf("regenerate: %v", err)
	}
	if v := f.n.View(); v.Masked != "https://hooks.example.com/…" || !v.Set {
		t.Errorf("view: %+v", v)
	}
	if err := f.n.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	if cfg, _ := f.n.current(); cfg.URL != "" || cfg.Secret != "" {
		t.Error("remove left the webhook")
	}
	if _, err := f.n.Regenerate(ctx); !errors.Is(err, ErrNoWebhook) {
		t.Errorf("regenerate with no webhook: %v", err)
	}
	// A refused URL saves nothing.
	if _, err := f.n.Save(ctx, Change{URL: "http://hooks.example.com/secret-path"}); err == nil || strings.Contains(err.Error(), "secret-path") {
		t.Errorf("a refused URL: %v", err)
	}
	if cfg, _ := f.n.current(); cfg.URL != "" {
		t.Error("a refused URL was saved")
	}
	f.forbidden = append(f.forbidden, first, next, "/a/one", "/a/two")
}

func TestLinkBase(t *testing.T) {
	for in, want := range map[string]string{
		"":                              "",
		"https://pier.example.com":      "https://pier.example.com",
		"https://pier.example.com/":     "https://pier.example.com",
		"http://192.168.1.4:7437":       "http://192.168.1.4:7437",
		"https://x.example/browserpier": "https://x.example/browserpier",
	} {
		got, err := CheckLinkBase(in)
		if err != nil || got != want {
			t.Errorf("CheckLinkBase(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"pier.example.com", "ftp://x", "https://u:p@x", "https://x/?a=1", "https://x/#f", "https://x/<b>", "https://x/a&b", "javascript:alert(1)"} {
		if _, err := CheckLinkBase(in); err == nil {
			t.Errorf("CheckLinkBase(%q) accepted", in)
		}
	}
}

// TestTheDialCheck: the address rules hold on the address dialled, not
// only when the URL was saved.
func TestTheDialCheck(t *testing.T) {
	rc := newReceiver(t)
	_, port, _ := strings.Cut(strings.TrimPrefix(rc.URL, "http://"), ":")
	var answer atomic.Value
	answer.Store("127.0.0.1")
	lookup := func(_ context.Context, host string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr(answer.Load().(string))}, nil
	}
	f := newNotifier(t, Options{Lookup: lookup})
	f.set("http://hooks.example.com:" + port + "/x") // private when saved
	for _, a := range []string{"169.254.169.254", "93.184.215.14"} {
		answer.Store(a)
		status, err := f.n.SendTest(context.Background())
		if err == nil || err.Error() != "address not allowed" {
			t.Errorf("resolving to %s: %d, %v", a, status, err)
		}
	}
	answer.Store("127.0.0.1")
	if status, err := f.n.SendTest(context.Background()); err != nil || status != 200 {
		t.Errorf("back to loopback: %d, %v", status, err)
	}
	if n := len(rc.posts()); n != 1 {
		t.Errorf("the receiver got %d POSTs, want 1", n)
	}
	// https:// to a name that now resolves to a link-local address.
	answer.Store("169.254.169.254")
	f.n.cfg.URL = "https://hooks.example.com/x"
	if _, err := f.n.SendTest(context.Background()); err == nil || err.Error() != "address not allowed" {
		t.Errorf("https to link-local: %v", err)
	}
}

func TestSendTest(t *testing.T) {
	f := newNotifier(t, Options{})
	if _, err := f.n.SendTest(context.Background()); !errors.Is(err, ErrNoWebhook) {
		t.Errorf("no webhook: %v", err)
	}
	rc := newReceiver(t, 418, 200)
	secret := f.set(rc.URL + "/x")
	status, err := f.n.SendTest(context.Background())
	if err != nil || status != 418 {
		t.Errorf("SendTest = %d, %v", status, err)
	}
	time.Sleep(100 * time.Millisecond)
	got := rc.posts()
	if len(got) != 1 {
		t.Fatalf("%d POSTs, want 1: no retry", len(got))
	}
	var body map[string]any
	_ = json.Unmarshal(got[0].body, &body)
	if body["event"] != "test" || body["text"] != body["content"] {
		t.Errorf("body: %s", got[0].body)
	}
	if !verify(secret, got[0].header.Get(SignatureHeader), got[0].body, f.n.now()) {
		t.Error("the test's signature doesn't verify")
	}
	// An unreachable host: a short error, never the URL.
	rc.Close()
	_, err = f.n.SendTest(context.Background())
	if err == nil || strings.Contains(err.Error(), "/x") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Errorf("unreachable: %v", err)
	}
}

// TestNoProxyFromTheEnvironment: HTTP_PROXY is ignored, so the address
// check sees the real address.
func TestNoProxyFromTheEnvironment(t *testing.T) {
	proxied := newReceiver(t)
	t.Setenv("HTTP_PROXY", proxied.URL)
	t.Setenv("HTTPS_PROXY", proxied.URL)
	t.Setenv("http_proxy", proxied.URL)
	t.Setenv("NO_PROXY", "")
	f := newNotifier(t, Options{})
	rc := newReceiver(t)
	f.set(rc.URL + "/x")
	if status, err := f.n.SendTest(context.Background()); err != nil || status != 200 {
		t.Fatalf("SendTest = %d, %v", status, err)
	}
	if len(rc.posts()) != 1 || len(proxied.posts()) != 0 {
		t.Errorf("direct %d, through the proxy %d", len(rc.posts()), len(proxied.posts()))
	}
}

// TestTheLogHoldsNoSecret: a token and a path pass through every log
// line above without appearing; here, a whole run with each outcome.
func TestTheLogHoldsNoSecret(t *testing.T) {
	f := newNotifier(t, Options{PublicURL: "https://pier.example.com"})
	rc := newReceiver(t, 500, 404)
	f.set(rc.URL + "/T00/B00/XXXXsecretXXXX?q=also-secret")
	f.forbidden = append(f.forbidden, "XXXXsecretXXXX", "also-secret", "tok-123-secret", "/h/")
	f.run()
	f.n.HandoffRequested(testHandoff, "tok-123-secret", func() bool { return true })
	rc.settle(t, 2)
	_, _ = f.n.SendTest(context.Background())
	rc.Close()
	_, _ = f.n.SendTest(context.Background())
}
