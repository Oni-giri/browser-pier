package static

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHeader(t *testing.T) {
	body := []byte("body{}")
	tag := Tag(body)
	serve := func(target string, header ...string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, target, nil)
		for i := 0; i+1 < len(header); i += 2 {
			r.Header.Set(header[i], header[i+1])
		}
		w := httptest.NewRecorder()
		Header(w, r, tag)
		http.ServeContent(w, r, "app.css", time.Time{}, bytes.NewReader(body))
		return w
	}
	if w := serve("/static/app.css?v=" + tag); w.Header().Get("Cache-Control") != "public, max-age=31536000, immutable" || w.Body.String() != "body{}" {
		t.Fatalf("versioned: %q %q", w.Header().Get("Cache-Control"), w.Body)
	}
	for _, target := range []string{"/static/app.css", "/static/app.css?v=old"} {
		if w := serve(target); w.Header().Get("Cache-Control") != "no-cache" || w.Header().Get("ETag") != `"`+tag+`"` {
			t.Fatalf("%s: %q %q", target, w.Header().Get("Cache-Control"), w.Header().Get("ETag"))
		}
	}
	if w := serve("/static/app.css", "If-None-Match", `"`+tag+`"`); w.Code != http.StatusNotModified || w.Body.Len() != 0 {
		t.Fatalf("revalidation: %d, %d bytes", w.Code, w.Body.Len())
	}
	if w := serve("/static/app.css", "If-None-Match", `"stale"`); w.Code != http.StatusOK {
		t.Fatalf("a stale copy: %d", w.Code)
	}
	if Tag([]byte("other")) == tag || len(tag) != 12 {
		t.Fatalf("tag %q", tag)
	}
}
