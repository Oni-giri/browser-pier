package ui

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/auth"
)

type agentsData struct {
	Tokens   []auth.Token
	Profiles []string
	PINSet   bool
	Base     string
	// The new-token dialog, when it has something to say.
	Form tokenForm
}

type tokenForm struct {
	Name     string
	All      bool
	Profiles []string
	Chosen   map[string]bool
	Error    string
	// Secret is the new token, shown once.
	Secret string
}

func (u *UI) agentsData(r *http.Request) (agentsData, error) {
	d := agentsData{Base: u.baseURL(r)}
	toks, err := u.web.Tokens().List(r.Context())
	if err != nil {
		return d, err
	}
	slices.SortFunc(toks, func(a, b auth.Token) int { return cmp.Compare(a.Name, b.Name) })
	d.Tokens = toks
	all, err := u.m.Profiles().List(r.Context())
	if err != nil {
		return d, err
	}
	for _, p := range all {
		d.Profiles = append(d.Profiles, p.Name)
	}
	changed, err := u.web.Admin().PINChangedAt(r.Context())
	d.PINSet = err == nil && !changed.IsZero()
	d.Form = tokenForm{All: true, Profiles: d.Profiles}
	return d, nil
}

// AgentMessage is what to paste into an agent's chat: it points the agent
// to the setup guide, which works for any client. The token stays in
// PIER_TOKEN, out of the chat.
func (d agentsData) AgentMessage() string {
	return "Connect yourself to my BrowserPier: read " + d.Base + "/mcp/setup (with curl if you can) and follow it. My agent token is in the PIER_TOKEN environment variable."
}

func (u *UI) agentsPage(w http.ResponseWriter, r *http.Request) {
	d, err := u.agentsData(r)
	if err != nil {
		u.log.Error("listing tokens", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	u.render(w, r, http.StatusOK, "agents", "Agents", "agents", d)
}

// createToken makes an agent token and shows it once, in the dialog.
func (u *UI) createToken(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 64<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	d, err := u.agentsData(r)
	if err != nil {
		u.log.Error("listing tokens", "err", err)
		http.Error(w, "something went wrong", http.StatusInternalServerError)
		return
	}
	f := tokenForm{Name: strings.TrimSpace(r.PostFormValue("name")), All: r.PostFormValue("scope") != "some",
		Profiles: d.Profiles, Chosen: map[string]bool{}}
	var chosen []string
	for _, p := range r.PostForm["profile"] {
		if slices.Contains(d.Profiles, p) && !f.Chosen[p] {
			f.Chosen[p] = true
			chosen = append(chosen, p)
		}
	}
	switch {
	case f.Name == "":
		f.Error = "Give the token a name, such as the agent's."
	case len(f.Name) > 64:
		f.Error = "Keep the name under 64 characters."
	case !f.All && len(chosen) == 0:
		f.Error = "Choose at least one profile, or allow all of them."
	}
	if f.Error == "" {
		if f.All {
			chosen = nil
		}
		_, secret, err := u.web.Tokens().Create(r.Context(), f.Name, f.All, chosen)
		if err != nil {
			u.log.Error("creating a token", "err", err)
			f.Error = "Couldn't create the token."
		} else {
			u.log.Info("agent token created from the UI", "name", f.Name, "all_profiles", f.All)
			f.Secret = secret
		}
	}
	status := http.StatusOK
	if f.Error != "" {
		status = http.StatusUnprocessableEntity
	}
	u.fragment(w, status, "agents", "token-dialog", f)
}

func (u *UI) revokeToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	tok, err := u.web.Tokens().Get(r.Context(), id)
	if err != nil {
		done(w, r, "/agents", "That token is already gone", false)
		return
	}
	if err := u.web.Tokens().Revoke(r.Context(), id); err != nil {
		u.log.Error("revoking a token", "err", err)
		done(w, r, "/agents", "Couldn't revoke "+tok.Name, true)
		return
	}
	// Its open CDP connections close, and its page calls and input
	// actions under way break off, at once.
	u.m.DisconnectToken(id)
	u.log.Info("agent token revoked from the UI", "name", tok.Name)
	done(w, r, "/agents", tok.Name+" revoked", false)
}

// lastUsed says when a token was last used.
func lastUsed(t time.Time) string {
	if t.IsZero() {
		return "Never"
	}
	s := ago(t)
	return strings.ToUpper(s[:1]) + s[1:]
}
