// Package theme is the light or dark look a device chooses in Settings. The
// choice is kept in a cookie on that device, and each page renders it on
// <html> so it shows from the first paint, with no script.
package theme

import (
	"html/template"
	"net/http"
	"time"
)

// Theme is "light", "dark", or System to follow the device's own setting.
type Theme string

const (
	System Theme = ""
	Light  Theme = "light"
	Dark   Theme = "dark"
)

// Cookie holds a device's choice. Without it, pages follow the system.
const Cookie = "bp_theme"

// Of is the theme r's device chose. Anything but light or dark is System.
func Of(r *http.Request) Theme {
	c, err := r.Cookie(Cookie)
	if err != nil {
		return System
	}
	if t := Theme(c.Value); t == Light || t == Dark {
		return t
	}
	return System
}

// Parse reads a choice from the Settings form: system, light or dark.
func Parse(s string) (Theme, bool) {
	switch s {
	case "system":
		return System, true
	case "light":
		return Light, true
	case "dark":
		return Dark, true
	}
	return System, false
}

// Set keeps t on this device; System forgets the choice.
func Set(w http.ResponseWriter, t Theme) {
	// Not Secure: it holds no secret, and plain-HTTP setups on a private
	// network couldn't keep a Secure cookie.
	c := &http.Cookie{Name: Cookie, Value: string(t), Path: "/", MaxAge: int((365 * 24 * time.Hour).Seconds()), HttpOnly: true, SameSite: http.SameSiteLaxMode} //nolint:gosec // see above
	if t == System {
		c.Value, c.MaxAge = "", -1
	}
	http.SetCookie(w, c)
}

// The browser's own bar, in each theme's page colour.
const (
	lightBar = "#EAF1FF"
	darkBar  = "#070C18"
)

// Head is the theme's <meta> tags: the colour schemes the page supports, so
// native controls and scrollbars match, and the colour of the browser's bar.
func (t Theme) Head() template.HTML {
	switch t {
	case Light:
		return `<meta name="color-scheme" content="light"><meta name="theme-color" content="` + lightBar + `">`
	case Dark:
		return `<meta name="color-scheme" content="dark"><meta name="theme-color" content="` + darkBar + `">`
	}
	return `<meta name="color-scheme" content="light dark">` +
		`<meta name="theme-color" content="` + lightBar + `" media="(prefers-color-scheme: light)">` +
		`<meta name="theme-color" content="` + darkBar + `" media="(prefers-color-scheme: dark)">`
}
