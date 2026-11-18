package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/fence"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// A page's file choosers (PROJ-152). The browser container answers them
// through a desktop portal of its own, so no dialog shows on the screen:
// a chooser waits, listed by GET /api/sessions/{id}/chooser, until it is
// answered with files from the profile's uploads or cancelled
// (POST …/chooser), the page closes it, or 5 minutes pass. Answering is
// input, as a click is; listing isn't.

// ChooserHeader carries the chooser notice of a Visual-mode action (the
// same line act's answer ends with) when a chooser is open after it.
const ChooserHeader = "X-File-Chooser"

const (
	// maxAcceptPatterns and maxAcceptLen cap what a chooser reports of
	// the page's accept list (the browser caps it too).
	maxAcceptPatterns = 32
	maxAcceptLen      = 64
	// maxChooserFiles is the most files one answer may give.
	maxChooserFiles = 10
)

// chooserJSON is one open chooser. Accept comes from the page: cleaned,
// capped, and fenced in the answer's text.
type chooserJSON struct {
	ID       string   `json:"id"`
	Multiple bool     `json:"multiple"`
	Accept   []string `json:"accept"`
}

var errNoPortal = errors.New("the file chooser portal isn't running in this browser; a page's file chooser shows the browser's own dialog")

// choosersOf asks the session's browser for its open choosers.
func (a *API) choosersOf(ctx context.Context, s sessions.Session) ([]chooserJSON, error) {
	resp, err := a.control(ctx, s.Profile, http.MethodGet, "/chooser", nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusServiceUnavailable, http.StatusNotFound: // an image from before the portal
		return nil, errNoPortal
	default:
		return nil, fmt.Errorf("the browser answered %d", resp.StatusCode)
	}
	var in struct {
		Choosers []chooserJSON `json:"choosers"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 256<<10)).Decode(&in); err != nil {
		return nil, err
	}
	out := make([]chooserJSON, 0, len(in.Choosers))
	for _, c := range in.Choosers {
		if !chooserID.MatchString(c.ID) {
			continue
		}
		out = append(out, chooserJSON{ID: c.ID, Multiple: c.Multiple, Accept: cleanAccept(c.Accept)})
	}
	a.noteOpened(s, out)
	return out, nil
}

var chooserID = regexp.MustCompile(`^c[0-9]{1,9}$`)

// cleanAccept cleans a chooser's patterns: what a person wouldn't see is
// gone (fence.Clean), and so is every control character and newline; at
// most maxAcceptPatterns of maxAcceptLen bytes.
func cleanAccept(in []string) []string {
	out := []string{}
	for _, p := range in {
		if len(out) == maxAcceptPatterns {
			break
		}
		p = strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, fence.Clean(strings.ToValidUTF8(p, "")))
		for len(p) > maxAcceptLen {
			_, n := utf8.DecodeLastRuneInString(p)
			p = p[:len(p)-n]
		}
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// choosersText is the open choosers in words, the page's accept lists
// fenced. sessionID names the endpoint that answers them.
func choosersText(sessionID string, cs []chooserJSON) string {
	if len(cs) == 0 {
		return "No file chooser is open."
	}
	var b, accepts strings.Builder
	fmt.Fprintf(&b, "File choosers open (answer with choose_files, or POST /api/sessions/%s/chooser, from the uploads):\n", sessionID)
	for _, c := range cs {
		how := "one file"
		if c.Multiple {
			how = fmt.Sprintf("up to %d files", maxChooserFiles)
		}
		fmt.Fprintf(&b, "%s: %s\n", c.ID, how)
		if len(c.Accept) > 0 {
			fmt.Fprintf(&accepts, "%s: %s\n", c.ID, strings.Join(c.Accept, " "))
		}
	}
	if accepts.Len() > 0 {
		b.WriteString("The types each accepts, as the page lists them (a hint; the browser doesn't enforce it):\n")
		b.WriteString(fence.Wrap("file chooser accept lists", accepts.String()))
	}
	return strings.TrimRight(b.String(), "\n")
}

// chooserNotice is the line an action's answer ends with while a chooser
// is open after it: BrowserPier's own words, nothing from the page. ""
// when none is open, or the browser can't say.
func (a *API) chooserNotice(ctx context.Context, s sessions.Session) string {
	cs, err := a.choosersOf(ctx, s)
	if err != nil || len(cs) == 0 {
		return ""
	}
	ids := make([]string, len(cs))
	for i, c := range cs {
		ids[i] = c.ID
	}
	if len(ids) == 1 {
		return fmt.Sprintf("a file chooser is open (%s): choose_files answers or cancels it", ids[0])
	}
	return fmt.Sprintf("file choosers are open (%s): choose_files answers or cancels each", strings.Join(ids, ", "))
}

// choosersSeen remembers the chooser ids a session's activity log has
// noted as opened.
type choosersSeen struct {
	mu   sync.Mutex
	seen map[string]map[string]bool
}

func (c *choosersSeen) forget(sessionID string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seen, sessionID)
}

// noteOpened logs each chooser not seen before, once.
func (a *API) noteOpened(s sessions.Session, cs []chooserJSON) {
	a.choosers.mu.Lock()
	if a.choosers.seen == nil {
		a.choosers.seen = map[string]map[string]bool{}
	}
	seen := a.choosers.seen[s.ID]
	if seen == nil {
		seen = map[string]bool{}
		a.choosers.seen[s.ID] = seen
	}
	var fresh int
	for _, c := range cs {
		if !seen[c.ID] {
			seen[c.ID] = true
			fresh++
		}
	}
	a.choosers.mu.Unlock()
	for range fresh {
		a.m.Note(s.ID, "", "The page opened a file chooser")
	}
}

// listChoosers is GET /api/sessions/{id}/chooser. It is a read, allowed
// during a handoff: it says nothing of what the person does. It doesn't
// count as the agent using the browser.
func (a *API) listChoosers(w http.ResponseWriter, r *http.Request) {
	s, ok := a.running(w, r, false)
	if !ok {
		return
	}
	cs, err := a.choosersOf(r.Context(), s)
	switch {
	case errors.Is(err, errNoPortal):
		writeError(w, http.StatusServiceUnavailable, err.Error())
		return
	case err != nil:
		writeError(w, http.StatusBadGateway, "couldn't ask the browser: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"choosers": cs, "text": choosersText(s.ID, cs)})
}

type chooseJSON struct {
	ID     string   `json:"id"`
	Files  []string `json:"files"`
	Cancel bool     `json:"cancel"`
}

// choose is POST /api/sessions/{id}/chooser: answer a chooser with files
// from the profile's uploads (bare names), or cancel it. It is input: it
// goes through AgentInput, so it is refused while a person has control
// or during a handoff.
func (a *API) choose(w http.ResponseWriter, r *http.Request) {
	var in chooseJSON
	if !readJSON(w, r, &in) {
		return
	}
	switch {
	case !chooserID.MatchString(in.ID):
		writeError(w, http.StatusBadRequest, "id: a file chooser's id, like c3, from GET …/chooser")
		return
	case in.Cancel && len(in.Files) > 0:
		writeError(w, http.StatusBadRequest, "send files or cancel, not both")
		return
	case !in.Cancel && len(in.Files) == 0:
		writeError(w, http.StatusBadRequest, "files: name at least one uploaded file, or send cancel: true")
		return
	case len(in.Files) > maxChooserFiles:
		writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("at most %d files at once", maxChooserFiles))
		return
	}
	for _, name := range in.Files {
		if !files.ValidName(name) {
			writeError(w, http.StatusBadRequest, "files: plain names of uploaded files, like report.pdf: no folders, not starting with a dot")
			return
		}
	}
	s, ok := a.running(w, r, true)
	if !ok {
		return
	}
	ctx, done, err := a.m.AgentInput(r.Context(), s.ID)
	if errors.Is(err, sessions.ErrInputBusy) {
		writeError(w, http.StatusConflict, "another input is still running in this browser; wait for it to finish, then send this one")
		return
	}
	if err != nil {
		writeError(w, http.StatusConflict, "a person has this browser (live view or a handoff); wait until they hand it back")
		return
	}
	defer done()
	// Only an upload, a regular file directly in the profile's uploads
	// folder, opened through os.Root without following a link, is named
	// to the browser, which checks it again on its side.
	if a.files == nil {
		writeError(w, http.StatusServiceUnavailable, "this BrowserPier keeps no files")
		return
	}
	for _, name := range in.Files {
		if _, _, err := a.files.Uploaded(s.Profile, name); err != nil {
			writeError(w, http.StatusUnprocessableEntity, fmt.Sprintf("no uploaded file named %q: send it first with upload_file (POST /api/sessions/%s/files)", name, s.ID))
			return
		}
	}
	body, _ := json.Marshal(map[string]any{"files": in.Files})
	if in.Cancel {
		body = []byte(`{"cancel":true}`)
	}
	resp, err := a.control(ctx, s.Profile, http.MethodPost, "/chooser/"+in.ID, body)
	if err != nil {
		writeError(w, http.StatusBadGateway, "couldn't reach the browser: "+err.Error())
		return
	}
	defer func() { _ = resp.Body.Close() }()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
	case http.StatusNotFound:
		writeError(w, http.StatusConflict, "that file chooser closed (answered, cancelled, closed by the page, or timed out); GET …/chooser lists the open ones")
		return
	case http.StatusServiceUnavailable:
		writeError(w, http.StatusServiceUnavailable, errNoPortal.Error())
		return
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		// The browser's own words about our request (a name, a count).
		writeError(w, resp.StatusCode, strings.TrimSpace(string(msg)))
		return
	default:
		writeError(w, http.StatusBadGateway, fmt.Sprintf("the browser answered %d", resp.StatusCode))
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	var what string
	switch {
	case in.Cancel:
		what = "Cancelled a file chooser"
	case len(in.Files) == 1:
		what = "Answered a file chooser with 1 file"
	default:
		what = fmt.Sprintf("Answered a file chooser with %d files", len(in.Files))
	}
	a.m.Note(s.ID, tok.Name, what)
	text := fmt.Sprintf("Answered the file chooser %s with %s.", in.ID, plural(len(in.Files), "file"))
	if in.Cancel {
		text = fmt.Sprintf("Cancelled the file chooser %s.", in.ID)
	}
	writeJSON(w, http.StatusOK, map[string]string{"text": text})
}

func plural(n int, what string) string {
	if n == 1 {
		return "1 " + what
	}
	return fmt.Sprintf("%d %ss", n, what)
}

// setChooserNotice sets a Visual-mode action's chooser header.
func (a *API) setChooserNotice(w http.ResponseWriter, r *http.Request, s sessions.Session) {
	if line := a.chooserNotice(r.Context(), s); line != "" {
		w.Header().Set(ChooserHeader, line)
	}
}
