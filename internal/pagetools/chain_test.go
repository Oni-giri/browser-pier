package pagetools

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

// hops sends /hop1 to /hop2 to /landed by script, and /loop to itself
// with a new query, forever.
func hops(u string) string {
	switch {
	case strings.HasSuffix(u, "/hop1"):
		return strings.TrimSuffix(u, "1") + "2"
	case strings.HasSuffix(u, "/hop2"):
		return strings.TrimSuffix(u, "hop2") + "landed"
	case strings.Contains(u, "/loop"):
		base, n, _ := strings.Cut(u, "?n=")
		i, _ := strconv.Atoi(n)
		return base + "?n=" + strconv.Itoa(i+1)
	}
	return ""
}

func TestNavigateFollowsScriptRedirects(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.fb.scripted = hops
	// 1: the first page has moved on before navigate first looks; 3: it
	// moves on while navigate watches it.
	for _, reads := range []int{1, 3} {
		t.Run(fmt.Sprint(reads), func(t *testing.T) {
			f.fb.mu.Lock()
			f.fb.hopReads = reads
			f.fb.mu.Unlock()
			out, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: "https://example.com/hop1"})
			if err != nil {
				t.Fatal(err)
			}
			f.closedAfter()
			if !strings.Contains(out, "URL: https://example.com/landed\n") || !strings.Contains(out, "Title: Page https://example.com/landed\n") {
				t.Errorf("not the chain's last page:\n%s", out)
			}
			if !strings.Contains(out, "Redirected from: https://example.com/hop1\n") || strings.Contains(out, "still") {
				t.Errorf("the note:\n%s", out)
			}
			// The tab's refs are the landed page's.
			snap, err := f.tools.Snapshot(context.Background(), f.session(), f.who, SnapshotIn{})
			if err != nil || !strings.Contains(snap, "URL: https://example.com/landed\n") {
				t.Errorf("snapshot after: %v\n%s", err, snap)
			}
		})
	}
}

func TestNavigateStopsFollowingAtTheCap(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.fb.scripted, f.fb.hopReads = hops, 2
	f.tools.chainCap = 300 * time.Millisecond
	start := time.Now()
	out, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: "https://example.com/loop"})
	took := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	f.closedAfter()
	if took < 300*time.Millisecond || took > 3*time.Second {
		t.Errorf("took %v, with a 300 ms cap", took)
	}
	if !strings.Contains(out, "still redirecting after") || !strings.Contains(out, "URL: https://example.com/loop?n=") {
		t.Errorf("at the cap:\n%s", out)
	}
	// The session is still usable.
	f.fb.mu.Lock()
	f.fb.scripted = nil
	f.fb.mu.Unlock()
	out, err = f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: "https://example.com/plain"})
	if err != nil || !strings.Contains(out, "URL: https://example.com/plain\n") || strings.Contains(out, "still") {
		t.Errorf("after the loop: %v\n%s", err, out)
	}
}

func TestNavigateChainEndsWithTheCaller(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.fb.scripted, f.fb.hopReads = hops, 2
	f.tools.chainCap = time.Minute
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := f.tools.Navigate(ctx, f.session(), f.who, NavigateIn{URL: "https://example.com/loop"})
	if err == nil {
		t.Fatal("a cancelled navigate answered")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("took %v to stop after its caller left", took)
	}
	f.closedAfter()
}

func TestNavigateToAFragmentDoesntWait(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/page", loginPage(), true)
	f.tools.chainQuiet = 5 * time.Second
	start := time.Now()
	out, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: "https://example.com/page#section"})
	if err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("a fragment took %v", took)
	}
	if !strings.Contains(out, "URL: https://example.com/page#section\n") || strings.Contains(out, "Redirected") || strings.Contains(out, "still") {
		t.Errorf("a fragment:\n%s", out)
	}
	f.closedAfter()
}

func TestNavigateNamesTheAddressThatFailed(t *testing.T) {
	f := newFixture(t)
	f.fb.addTab("https://example.com/", loginPage(), true)
	f.fb.redirects = map[string]string{"https://example.com/sso": "https://idp.example/auth"}
	f.fb.fails = map[string]string{"https://idp.example/auth": "net::ERR_NAME_NOT_RESOLVED"}
	f.fb.scripted, f.fb.hopReads = func(u string) string {
		if u == "https://example.com/check" {
			return "https://idp.example/auth"
		}
		return ""
	}, 2
	nav := func(u string) *Error {
		t.Helper()
		out, err := f.tools.Navigate(context.Background(), f.session(), f.who, NavigateIn{URL: u})
		f.closedAfter()
		var e *Error
		if !errors.As(err, &e) || e.Status != 502 {
			t.Fatalf("%s: %v\n%s", u, err, out)
		}
		return e
	}
	// A server redirect to an address that fails: both are named.
	e := nav("https://example.com/sso")
	if !strings.Contains(e.Msg, "after a redirect") || !strings.Contains(e.Msg, "net::ERR_NAME_NOT_RESOLVED") || !strings.Contains(e.Msg, "Failed at: https://idp.example/auth") {
		t.Errorf("server redirect: %s", e.Msg)
	}
	if i, j := strings.Index(e.Msg, "Failed at"), strings.LastIndex(e.Msg, "</untrusted-content-"); i < 0 || i > j {
		t.Errorf("the address is outside the fence: %s", e.Msg)
	}
	// The address asked for itself: nothing more to say.
	if e := nav("https://idp.example/auth"); strings.Contains(e.Msg, "Failed at") || strings.Contains(e.Msg, "redirect") || !strings.Contains(e.Msg, "net::ERR_NAME_NOT_RESOLVED") {
		t.Errorf("direct: %s", e.Msg)
	}
	// A script redirect that fails, during the chain.
	if e := nav("https://example.com/check"); !strings.Contains(e.Msg, "after a redirect") || !strings.Contains(e.Msg, "Failed at: https://idp.example/auth") {
		t.Errorf("script redirect: %s", e.Msg)
	}
}
