package api

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/scrub"
)

// envelopeSecret has a "<" and an "&", which the envelope writes as
// "&lt;" in the URL and title and leaves in the page text.
const envelopeSecret = `Se<cret&Value42`

// A secret in the page envelope's URL, title and content is hidden in all
// three (PROJ-167): the scrub stays the last step, over the finished
// answer, and matches the escaped forms too.
func TestPageEnvelopeIsScrubbed(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "api")
	if err := f.m.RegisterSecret(s.ID, envelopeSecret); err != nil {
		t.Fatal(err)
	}
	page := fence.WrapPage("page snapshot", fence.Page{
		URL:     "https://x.example/?p=" + url.QueryEscape(envelopeSecret),
		Title:   "Account " + envelopeSecret,
		Format:  "outline",
		Content: "Your key is " + envelopeSecret,
	})
	srv := f.scrubServer(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": page})
	})
	_, _, body := get(t, srv, f.all)
	var out struct{ Text string }
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{envelopeSecret, "Se&lt;cret", url.QueryEscape(envelopeSecret), "cret&Value"} {
		if strings.Contains(out.Text, leak) {
			t.Errorf("%q leaked:\n%s", leak, out.Text)
		}
	}
	for _, field := range []string{"<url>", "<title>", "<page_content-"} {
		i := strings.Index(out.Text, field)
		if i < 0 {
			t.Fatalf("no %s:\n%s", field, out.Text)
		}
		end := strings.Index(out.Text[i:], "\n")
		if field == "<page_content-" {
			end = strings.Index(out.Text[i:], "\n</")
		}
		if !strings.Contains(out.Text[i:i+end], scrub.Hidden) {
			t.Errorf("%s isn't hidden:\n%s", field, out.Text)
		}
	}
}
