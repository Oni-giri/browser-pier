package ui

import (
	"bytes"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/static"
)

// iconTags remembers each cached icon's tag by its file's size and time,
// so pages listing many profiles don't hash every icon each time.
var iconTags sync.Map // profile name → iconTag

type iconTag struct {
	size int64
	mod  time.Time
	tag  string
}

// readIcon returns a profile's cached icon and its tag (see static.Tag);
// nil if it has none.
func (u *UI) readIcon(name string) ([]byte, string, time.Time) {
	path := u.m.Layout().IconFile(name)
	b, err := os.ReadFile(path) //nolint:gosec // an existing profile's name: [a-z0-9-] only
	if err != nil {
		return nil, "", time.Time{}
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, "", time.Time{}
	}
	tag := static.Tag(b)
	iconTags.Store(name, iconTag{size: st.Size(), mod: st.ModTime(), tag: tag})
	return b, tag, st.ModTime()
}

// IconTag is the version of a profile's cached icon (the favicon read from
// its browser), "" if it has none, so pages show the fallback glyph. It
// changes whenever the icon does.
func (u *UI) IconTag(name string) string {
	st, err := os.Stat(u.m.Layout().IconFile(name))
	if err != nil {
		iconTags.Delete(name)
		return ""
	}
	if v, ok := iconTags.Load(name); ok {
		if t := v.(iconTag); t.size == st.Size() && t.mod.Equal(st.ModTime()) {
			return t.tag
		}
	}
	_, tag, _ := u.readIcon(name)
	return tag
}

// IconURL links a profile's cached icon with its version, so the browser
// keeps it until it changes; "" if it has none.
func (u *UI) IconURL(name string) string {
	tag := u.IconTag(name)
	if tag == "" {
		return ""
	}
	return "/profiles/" + url.PathEscape(name) + "/icon?v=" + tag
}

// icon serves a profile's cached icon: a PNG BrowserPier drew itself from
// the browser's favicon cache, never the site's own file. It says which
// sites a profile uses, so only the signed-in admin gets it.
func (u *UI) icon(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, err := u.m.Profiles().Get(r.Context(), name); err != nil {
		http.NotFound(w, r)
		return
	}
	b, tag, mod := u.readIcon(name)
	if b == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Content-Security-Policy", "default-src 'none'")
	w.Header().Set("ETag", `"`+tag+`"`)
	http.ServeContent(w, r, "", mod, bytes.NewReader(b))
}
