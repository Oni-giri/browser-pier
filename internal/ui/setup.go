package ui

import (
	"net/http"
	"text/template"
)

// setupGuide tells an agent how to connect itself to BrowserPier, with
// this BrowserPier's address filled in.
var setupGuide = template.Must(template.ParseFS(embedded, "templates/setup.md"))

// setup serves the guide the Agent tab's prompt points agents to. It holds
// no secret, so it needs no sign-in: the agent reads its token from
// PIER_TOKEN, never from here.
func (u *UI) setup(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	if err := setupGuide.Execute(w, struct{ Base string }{u.baseURL(r)}); err != nil {
		u.log.Error("writing the setup guide", "err", err)
	}
}
