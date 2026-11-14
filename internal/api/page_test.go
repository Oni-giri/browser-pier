package api

import (
	"net/http"
	"strings"
	"testing"
)

func TestPageToolRoutes(t *testing.T) {
	f := newFixture(t)
	visual := f.start(f.all, "google", "visual")
	other := f.start(f.all, "github", "api")
	bodies := map[string]string{"snapshot": `{}`, "find": `{"text": "x"}`, "navigate": `{"url": "https://example.com"}`,
		"act": `{"steps": [{"action": "press", "keys": "Enter"}]}`, "wait": `{"text": "x"}`, "tabs": `{"action": "list"}`}
	for tool, body := range bodies {
		path := "/api/sessions/" + visual.ID + "/" + tool
		if code := f.call("", http.MethodPost, path, body, nil); code != http.StatusUnauthorized {
			t.Errorf("%s without a token: %d", tool, code)
		}
		var e map[string]string
		if code := f.call(f.all, http.MethodPost, path, body, &e); code != http.StatusConflict || !strings.Contains(e["error"], "Visual mode") {
			t.Errorf("%s in Visual mode: %d %v", tool, code, e)
		}
		// A token scoped to another profile doesn't see the session.
		if code := f.call(f.scoped, http.MethodPost, "/api/sessions/"+other.ID+"/"+tool, body, &e); code != http.StatusNotFound {
			t.Errorf("%s on another profile's session: %d %v", tool, code, e)
		}
		if code := f.call(f.all, http.MethodPost, "/api/sessions/nope/"+tool, body, nil); code != http.StatusNotFound {
			t.Errorf("%s on no session: %d", tool, code)
		}
		if code := f.call(f.all, http.MethodPost, path, `{"nonsense": 1}`, &e); code != http.StatusBadRequest {
			t.Errorf("%s with an unknown field: %d %v", tool, code, e)
		}
	}
	// An empty body is the defaults.
	var e map[string]string
	if code := f.call(f.all, http.MethodPost, "/api/sessions/"+visual.ID+"/snapshot", "", &e); code != http.StatusConflict {
		t.Errorf("snapshot with no body: %d %v", code, e)
	}
}

func TestContentRoutes(t *testing.T) {
	f := newFixture(t)
	visual := f.start(f.all, "google", "visual")
	other := f.start(f.all, "github", "api")
	path := "/api/sessions/" + visual.ID + "/content"
	if code := f.call("", http.MethodPost, path, `{"format": "markdown"}`, nil); code != http.StatusUnauthorized {
		t.Errorf("without a token: %d", code)
	}
	var e map[string]string
	for _, format := range []string{"markdown", "readability", "pdf"} {
		body := `{"format": "` + format + `"}`
		if code := f.call(f.all, http.MethodPost, path, body, &e); code != http.StatusConflict || !strings.Contains(e["error"], "Visual mode") {
			t.Errorf("%s in Visual mode: %d %v", format, code, e)
		}
		if code := f.call(f.scoped, http.MethodPost, "/api/sessions/"+other.ID+"/content", body, &e); code != http.StatusNotFound {
			t.Errorf("%s on another profile's session: %d %v", format, code, e)
		}
	}
	// A screenshot comes from the X display, in Visual mode too.
	r, png := f.raw(f.all, http.MethodPost, path, strings.NewReader(`{"format": "screenshot", "width": 640}`), "application/json")
	if r.StatusCode != http.StatusOK || png != "PNG:640" || r.Header.Get("Content-Type") != "image/png" {
		t.Errorf("screenshot: %d %q %v", r.StatusCode, png, r.Header)
	}
	if acts := f.m.Activity(visual.ID); len(acts) == 0 || acts[0].What != "Took a screenshot" {
		t.Errorf("activity: %+v", acts)
	}
	for _, body := range []string{``, `{}`, `{"format": "html"}`, `{"format": "markdown", "nonsense": 1}`,
		`{"format": "markdown", "width": 640}`, `{"format": "readability", "pages": "1"}`, `{"format": "screenshot", "max_chars": 10}`,
		`{"format": "screenshot", "width": 8}`, `{"format": "pdf", "offset": 10}`} {
		if code := f.call(f.all, http.MethodPost, path, body, &e); code != http.StatusBadRequest {
			t.Errorf("%s: %d %v", body, code, e)
		}
	}
}
