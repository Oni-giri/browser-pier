//go:build docker

// app.js in a real browser container: the behaviours htmx doesn't cover,
// with requests the test can hold, fail or let through. See
// internal/sessions/docker_test.go for the environment it needs.
package ui

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/handoff"
)

// held is a request the test holds: what it sent, and how to answer it.
type held struct {
	form    url.Values
	release chan http.HandlerFunc
}

// gate holds every request to the routes it's given until the test
// answers it.
type gate struct{ arrived chan held }

func (g *gate) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	h := held{r.PostForm, make(chan http.HandlerFunc)}
	g.arrived <- h
	(<-h.release)(w, r)
}

// next is the next held request.
func (g *gate) next(t *testing.T) held {
	t.Helper()
	select {
	case h := <-g.arrived:
		return h
	case <-time.After(10 * time.Second):
		t.Fatal("no request came")
		return held{}
	}
}

// settle fails whatever is still held when a subtest ends, so a failed
// subtest leaves nothing pending for the next one.
func (g *gate) settle() {
	for {
		select {
		case h := <-g.arrived:
			h.release <- failWith500
		case <-time.After(time.Second):
			return
		}
	}
}

// Ways to answer a held request.
func failWith500(w http.ResponseWriter, _ *http.Request) {
	http.Error(w, "Something broke.", http.StatusInternalServerError)
}

func dropConnection(w http.ResponseWriter, _ *http.Request) {
	c, _, err := w.(http.Hijacker).Hijack()
	if err == nil {
		_ = c.Close()
	}
}

// unreachable answers h as if BrowserPier were gone. Chrome sends a POST
// again when a kept-alive connection closes before any answer, so the
// retry is dropped too.
func (g *gate) unreachable(h held) {
	for {
		h.release <- dropConnection
		select {
		case h = <-g.arrived:
		case <-time.After(2 * time.Second):
			return
		}
	}
}

func TestAppJS(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	ctx := context.Background()
	f := newFixture(t)
	f.profile("mail")
	app := f.srv.Config.Handler
	g := &gate{arrived: make(chan held)}
	mux := http.NewServeMux()
	mux.Handle("POST /settings/appearance", g)
	mux.Handle("POST /profiles/{name}/delete", g)
	mux.Handle("/", app)
	base := serveToBrowser(t, mux)

	b := newPhone(t, image)
	b.Call("Emulation.setDeviceMetricsOverride", map[string]any{"width": 1440, "height": 900, "deviceScaleFactor": 1, "mobile": false})
	b.Call("Emulation.setEmulatedMedia", map[string]any{"features": []map[string]string{{"name": "prefers-color-scheme", "value": "light"}}})
	b.Navigate(base + "/login")
	b.Eval(`document.querySelector("#password").value = "the-admin-password"; document.querySelector("#password").form.requestSubmit()`)
	until(t, b, "signing in didn't leave the login page", `location.pathname === "/"`)

	theme := `(document.documentElement.dataset.theme || "system")`
	checked := `document.querySelector("form[data-appearance] input:checked").value`
	pick := func(v string) {
		b.Eval(fmt.Sprintf(`document.querySelector("form[data-appearance] input[value=%s]").click()`, v))
	}
	toast := func(text string) string {
		return fmt.Sprintf(`[...document.querySelectorAll("#toasts .toast")].some((t) => t.textContent.includes(%q))`, text)
	}

	t.Run("appearance switches at once and is kept", func(t *testing.T) {
		t.Cleanup(g.settle)
		b.Navigate(base + "/settings")
		pick("dark")
		// The page is dark while the save is still on its way.
		h := g.next(t)
		is(t, b, theme, "dark")
		if h.form.Get("theme") != "dark" {
			t.Fatalf("sent %v", h.form)
		}
		h.release <- app.ServeHTTP
		until(t, b, "no toast after saving", toast("Appearance saved"))
		b.Navigate(base + "/settings")
		is(t, b, theme, "dark")
		is(t, b, checked, "dark")
	})

	t.Run("a theme that couldn't be saved switches back", func(t *testing.T) {
		t.Cleanup(g.settle)
		b.Navigate(base + "/settings")
		for _, fail := range []func(held){
			func(h held) { h.release <- failWith500 },
			g.unreachable,
		} {
			pick("light")
			h := g.next(t)
			is(t, b, theme, "light")
			fail(h)
			until(t, b, "the page didn't switch back to the saved theme", theme+` === "dark" && `+checked+` === "dark"`)
		}
		until(t, b, "no toast for a lost connection", toast("Can't reach BrowserPier."))
	})

	t.Run("quick choices each keep their own answer", func(t *testing.T) {
		t.Cleanup(g.settle)
		// Saved: Dark. Light is held, Dark and then Light chosen meanwhile;
		// htmx keeps only the last one queued. Light fails, and as a newer
		// choice is on its way, the page doesn't switch back.
		b.Navigate(base + "/settings")
		b.Eval(`window.answers = 0; document.addEventListener("htmx:afterRequest", () => answers++)`)
		pick("light")
		first := g.next(t)
		pick("dark")
		pick("light")
		is(t, b, theme, "light")
		first.release <- failWith500
		until(t, b, "the failure never reached the page", `answers === 1`)
		is(t, b, theme, "light")
		is(t, b, checked, "light")
		last := g.next(t)
		if last.form.Get("theme") != "light" || last.form.Get("choice") != "3" {
			t.Fatalf("the queued request sent %v, want the third choice, light", last.form)
		}
		last.release <- app.ServeHTTP
		until(t, b, "no toast after saving", toast("Appearance saved"))
		b.Navigate(base + "/settings")
		is(t, b, theme, "light")
	})

	t.Run("unsaved changes bar", func(t *testing.T) {
		b.Navigate(base + "/settings")
		hidden := `document.querySelector("#unsaved").hidden`
		is(t, b, hidden, true)
		b.Eval(`document.querySelector("input[name=auto_stop][value='30']").click()`)
		is(t, b, hidden, false)
		b.Eval(`document.querySelector("[data-discard]").click()`)
		is(t, b, hidden, true)
		is(t, b, `document.querySelector("input[name=auto_stop][value='30']").checked`, false)
	})

	t.Run("dialogs fill from the button that opens them, and say they're busy", func(t *testing.T) {
		t.Cleanup(g.settle)
		b.Navigate(base + "/profiles")
		b.Eval(`document.querySelector("[data-dialog=delete-dialog][data-name=mail]").click()`)
		is(t, b, `document.querySelector("#delete-dialog").open`, true)
		is(t, b, `document.querySelector("#delete-title").textContent`, "Delete mail?")
		is(t, b, `new URL(document.querySelector("#delete-dialog form").action).pathname`, "/profiles/mail/delete")
		// Read in the same call: while the form's navigation is pending,
		// the page may not answer another one.
		busy := b.Eval(`(async () => {
			const form = document.querySelector("#delete-dialog form"), btn = form.querySelector("button[type=submit]");
			form.requestSubmit();
			await new Promise((r) => setTimeout(r, 50));
			return btn.disabled + " " + btn.textContent;
		})()`)
		h := g.next(t)
		if busy != "true Deleting…" {
			t.Errorf("the delete button is %q while it submits, want disabled and Deleting…", busy)
		}
		h.release <- failWith500 // keeps the profile for the next test
		until(t, b, "the error page didn't load", `document.readyState === "complete" && location.pathname === "/profiles/mail/delete"`)
	})

	t.Run("the page light follows the needs-you chip", func(t *testing.T) {
		b.Navigate(base + "/profiles")
		is(t, b, `document.body.classList.contains("has-help")`, false)
		s, _, err := f.m.Start(ctx, "mail", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := f.handoffs.Create(ctx, s.ID, "claude", handoff.SensitiveInput, "Sign in", ""); err != nil {
			t.Fatal(err)
		}
		// The chip polls every 5 s.
		untilFor(t, b, 15*time.Second, "the page didn't light up for a request", `document.body.classList.contains("has-help") && !!document.querySelector("#chip .chip-help")`)
	})
}

// is checks that expr is want, giving the page a moment to catch up.
func is(t *testing.T, b *browsertest.CDP, expr string, want any) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for got := b.Eval(expr); got != want; got = b.Eval(expr) {
		if time.Now().After(deadline) {
			t.Fatalf("%s is %v, want %v", expr, got, want)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func until(t *testing.T, b *browsertest.CDP, what, cond string) {
	t.Helper()
	untilFor(t, b, 10*time.Second, what, cond)
}

func untilFor(t *testing.T, b *browsertest.CDP, d time.Duration, what, cond string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for b.Eval(cond) != true {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(100 * time.Millisecond)
	}
}
