package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// DownloadsHeader carries the download notice of a Visual-mode click or
// key press (PROJ-116). The status stays what it was (204, or 200 with
// the screenshot), so no client that checks it breaks; the header is
// there only when the action started a download.
const DownloadsHeader = "X-Downloads"

// downloadsNotice is the header's JSON: at most files.MaxNotices files,
// and how many more there were.
type downloadsNotice struct {
	Files []files.Download `json:"files"`
	More  int              `json:"more,omitempty"`
}

// markDownloads lists the session's downloads folder before an action,
// if the API has a files store.
func (a *API) markDownloads(s sessions.Session) *files.Mark {
	if a.files == nil {
		return nil
	}
	return a.files.Mark(s.Profile, s.ID)
}

// reportDownloads compares the downloads folder with mark after an
// action, waiting up to wait for a download it started; it logs each
// finished file and sets the X-Downloads header if there is anything to
// say. It never fails the action.
func (a *API) reportDownloads(w http.ResponseWriter, r *http.Request, s sessions.Session, mark *files.Mark, wait time.Duration) {
	ds := mark.Downloads(r.Context(), wait)
	if len(ds) == 0 {
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	for _, line := range files.LogLines(ds) {
		a.m.Note(s.ID, tok.Name, line)
	}
	w.Header().Set(DownloadsHeader, downloadsHeader(ds))
}

// downloadsHeader is the notice as one line of ASCII JSON: a header
// can't carry raw UTF-8 safely, so anything past ASCII is \u-escaped
// (encoding/json already escapes control characters).
func downloadsHeader(ds []files.Download) string {
	n := downloadsNotice{Files: ds}
	if len(ds) > files.MaxNotices {
		n.Files, n.More = ds[:files.MaxNotices], len(ds)-files.MaxNotices
	}
	raw, err := json.Marshal(n)
	if err != nil {
		return `{"files":[]}`
	}
	return asciiJSON(string(raw))
}

func asciiJSON(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r < utf8.RuneSelf:
			b.WriteRune(r)
		case r > 0xFFFF:
			r -= 0x10000
			fmt.Fprintf(&b, `\u%04x\u%04x`, 0xD800+(r>>10), 0xDC00+(r&0x3FF))
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}
