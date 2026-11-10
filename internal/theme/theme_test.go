package theme

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOf(t *testing.T) {
	for cookie, want := range map[string]Theme{"": System, "light": Light, "dark": Dark, "Dark": System, "blue": System, `dark" onload="x`: System} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if cookie != "" {
			r.AddCookie(&http.Cookie{Name: Cookie, Value: cookie})
		}
		if got := Of(r); got != want {
			t.Errorf("cookie %q: %q, want %q", cookie, got, want)
		}
	}
}

func TestParse(t *testing.T) {
	for in, want := range map[string]Theme{"system": System, "light": Light, "dark": Dark} {
		if got, ok := Parse(in); !ok || got != want {
			t.Errorf("%q: %q %v", in, got, ok)
		}
	}
	for _, in := range []string{"", "auto", "Dark"} {
		if _, ok := Parse(in); ok {
			t.Errorf("%q was accepted", in)
		}
	}
}

func TestSet(t *testing.T) {
	w := httptest.NewRecorder()
	Set(w, Dark)
	c := w.Result().Cookies()[0]
	if c.Name != Cookie || c.Value != "dark" || c.Path != "/" || c.MaxAge < 300*24*3600 || c.SameSite != http.SameSiteLaxMode {
		t.Errorf("dark: %+v", c)
	}
	w = httptest.NewRecorder()
	Set(w, System)
	if c := w.Result().Cookies()[0]; c.Name != Cookie || c.MaxAge >= 0 {
		t.Errorf("system should clear the cookie: %+v", c)
	}
}

func TestHead(t *testing.T) {
	sys := string(System.Head())
	if !strings.Contains(sys, `content="light dark"`) || strings.Count(sys, `name="theme-color"`) != 2 || !strings.Contains(sys, `media="(prefers-color-scheme: dark)"`) {
		t.Errorf("system: %s", sys)
	}
	for th, want := range map[Theme]string{Light: lightBar, Dark: darkBar} {
		h := string(th.Head())
		if !strings.Contains(h, `name="color-scheme" content="`+string(th)+`"`) || strings.Count(h, `name="theme-color"`) != 1 || !strings.Contains(h, want) || strings.Contains(h, "media=") {
			t.Errorf("%s: %s", th, h)
		}
	}
}
