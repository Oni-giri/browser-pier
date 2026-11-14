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
