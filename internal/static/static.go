// Package static sets the caching headers for the files BrowserPier embeds,
// so a browser keeps them from one page to the next.
package static

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
)

// Tag is a file's version: the start of its SHA-256, in hex.
func Tag(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:6])
}

// Header sets a file's ETag and Cache-Control before it's served with
// http.ServeContent or a file server, which then answer a matching
// If-None-Match with 304. A link that carries the file's tag as ?v= changes
// whenever the file does, so it may be kept for a year; any other link has to
// check back each time. With no tag, for a file that isn't there, the answer
// isn't kept either.
func Header(w http.ResponseWriter, r *http.Request, tag string) {
	if tag == "" {
		w.Header().Set("Cache-Control", "no-cache")
		return
	}
	w.Header().Set("ETag", `"`+tag+`"`)
	if r.URL.Query().Get("v") == tag {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
}
