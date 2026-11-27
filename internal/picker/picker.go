// Package picker lets the person in control of a browser answer a page's
// file chooser (PROJ-181): the modal over the live view and the handoff
// page, with the profile's uploads to pick from and an upload from the
// person's own device.
//
// The browser container's portal holds every chooser (PROJ-152) and
// answers it only with files from the profile's uploads. No CDP is used:
// the page learns nothing it wouldn't from a desktop dialog. The routes
// that mount these handlers (internal/ui for the signed-in admin,
// internal/handoff for a handoff link's holder) find the session, say
// who may reach it and who is asking (a sessions holder id); the handlers
// here check that the one asking holds control of it (PROJ-184), as the
// live view's input does.
package picker

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"math"
	"mime"
	"net/http"
	"time"

	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// MaxUpload is the largest file a person may upload from their device,
// as for agents (the profile's files cap may be lower).
const MaxUpload = 100 << 20

// maxChosen is the most files one answer may give, as the browser allows.
const maxChosen = 10

//go:embed chooser.html
var chooserHTML string

var chooserTmpl = template.Must(template.New("chooser").Parse(chooserHTML))

// Picker serves the person's side of a page's file choosers.
type Picker struct {
	m     *sessions.Manager
	files *files.Store
	log   *slog.Logger
	now   func() time.Time
	max   int64 // MaxUpload; tests lower it
}

func New(m *sessions.Manager, fs *files.Store, log *slog.Logger) *Picker {
	return &Picker{m: m, files: fs, log: log, now: time.Now, max: MaxUpload}
}

// Activity log entries: no file names, as for agents' answers.
const (
	cancelled = "Cancelled a file chooser"
)

func chose(n int) string {
	if n == 1 {
		return "A person chose 1 file"
	}
	return fmt.Sprintf("A person chose %d files", n)
}

const errNoControl = "Take control of the browser first."

// modal is what the template shows of one chooser.
type modal struct {
	ID       string
	Multiple bool
	Files    []upload
	Left     string
}

type upload struct{ Name, Size, Ago string }

// Show answers the modal for the oldest chooser open in s's browser, or
// 204 when none is (or the browser has no portal), to holder if they hold
// control. It doesn't count as the person using the browser: the page
// asks every second.
func (p *Picker) Show(w http.ResponseWriter, r *http.Request, s sessions.Session, holder string) {
	if p == nil {
		http.Error(w, "file choosers can't be answered here", http.StatusServiceUnavailable)
		return
	}
	if !p.m.ControlledBy(s.ID, holder) {
		http.Error(w, errNoControl, http.StatusConflict)
		return
	}
	cs, err := p.m.Choosers(r.Context(), s.Profile)
	switch {
	case errors.Is(err, sessions.ErrNoPortal):
		w.WriteHeader(http.StatusNoContent)
		return
	case err != nil:
		p.log.Warn("listing a browser's file choosers", "profile", s.Profile, "err", err)
		http.Error(w, "Couldn't ask the browser.", http.StatusBadGateway)
		return
	case len(cs) == 0:
		w.WriteHeader(http.StatusNoContent)
		return
	}
	list, err := p.files.ListUploads(s.Profile)
	if err != nil {
		p.log.Error("listing uploads", "profile", s.Profile, "err", err)
		http.Error(w, "Couldn't list the uploads.", http.StatusInternalServerError)
		return
	}
	c := cs[0]
	d := modal{ID: c.ID, Multiple: c.Multiple, Left: left(c.Left)}
	for _, f := range list {
		d.Files = append(d.Files, upload{Name: f.Name, Size: size(f.Size), Ago: p.ago(f.Modified)})
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if err := chooserTmpl.Execute(w, d); err != nil {
		p.log.Error("rendering the file chooser", "err", err)
	}
}

// Answer answers a chooser in s's browser with uploads the person picked
// (form: id, file…) or cancels it (form: id, cancel). It is the person's
// input: refused unless holder holds control.
func (p *Picker) Answer(w http.ResponseWriter, r *http.Request, s sessions.Session, holder string) {
	if p == nil {
		http.Error(w, "file choosers can't be answered here", http.StatusServiceUnavailable)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Send the chooser's id, and files or cancel.", http.StatusBadRequest)
		return
	}
	id, names, cancel := r.PostFormValue("id"), r.PostForm["file"], r.PostFormValue("cancel") != ""
	switch {
	case cancel && len(names) > 0:
		http.Error(w, "Choose files or cancel, not both.", http.StatusBadRequest)
		return
	case !cancel && len(names) == 0:
		http.Error(w, "Pick a file first.", http.StatusBadRequest)
		return
	case len(names) > maxChosen:
		http.Error(w, fmt.Sprintf("At most %d files at once.", maxChosen), http.StatusUnprocessableEntity)
		return
	}
	// done once the answer has reached the browser: a hand-back waits
	// for it before emptying the clipboard (PROJ-194). ctx ends as the
	// takeover's hand-back begins, and the browser refuses an answer
	// for a takeover that has ended (turn, PROJ-196).
	ctx, turn, done, err := p.m.HumanInput(s.ID, holder)
	if err != nil {
		http.Error(w, errNoControl, http.StatusConflict)
		return
	}
	defer done()
	// Only what the uploads folder holds, as the agent's route checks,
	// before anything goes to the browser (which checks again).
	for _, name := range names {
		if !files.ValidName(name) {
			http.Error(w, "Pick one of the uploads listed.", http.StatusBadRequest)
			return
		}
		if _, _, err := p.files.Uploaded(s.Profile, name); err != nil {
			http.Error(w, "That upload is gone. Pick another, or upload one from this device.", http.StatusUnprocessableEntity)
			return
		}
	}
	p.answer(ctx, w, turn, s, id, names)
}

// Upload stores a file from the person's device in the profile's uploads
// (multipart: id, then file), through files.Upload with its name rules
// and caps, then answers chooser id with it. Refused unless holder holds
// control.
func (p *Picker) Upload(w http.ResponseWriter, r *http.Request, s sessions.Session, holder string) {
	if p == nil {
		http.Error(w, "file choosers can't be answered here", http.StatusServiceUnavailable)
		return
	}
	// Before reading what may be 100 MiB: is this person in control?
	if !p.m.ControlledBy(s.ID, holder) {
		http.Error(w, errNoControl, http.StatusConflict)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, p.max+1<<20)
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt != "multipart/form-data" {
		http.Error(w, "Send the file as a form.", http.StatusBadRequest)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "Send the file as a form.", http.StatusBadRequest)
		return
	}
	var id string
	for {
		part, err := mr.NextPart()
		if err != nil {
			http.Error(w, "Send the chooser's id, then the file.", http.StatusBadRequest)
			return
		}
		if part.FormName() == "id" {
			b, _ := io.ReadAll(io.LimitReader(part, 32))
			id = string(b)
			continue
		}
		if part.FormName() != "file" {
			continue
		}
		if id == "" {
			http.Error(w, "Send the chooser's id, then the file.", http.StatusBadRequest)
			return
		}
		// The upload answers a chooser: one must be open, before up to
		// 100 MiB is stored (and older files evicted for it).
		if !p.open(r, s, id) {
			http.Error(w, "That file chooser closed: the page closed it, or its time ran out.", http.StatusConflict)
			return
		}
		f, _, err := p.files.Upload(s.Profile, part.FileName(), part, p.max)
		switch {
		case errors.Is(err, files.ErrBadName):
			http.Error(w, "That file's name can't be used here: rename it, without slashes and not starting with a dot, then upload it again.", http.StatusBadRequest)
			return
		case errors.Is(err, files.ErrTooLarge):
			http.Error(w, fmt.Sprintf("That file is too large: over %d MiB, the most one upload may be here.", p.limit()>>20), http.StatusRequestEntityTooLarge)
			return
		case errors.Is(err, files.ErrOverCap):
			http.Error(w, "That file doesn't fit in this profile's files cap.", http.StatusInsufficientStorage)
			return
		case err != nil:
			var tooBig *http.MaxBytesError
			if errors.As(err, &tooBig) {
				http.Error(w, fmt.Sprintf("That file is too large: over %d MiB.", p.max>>20), http.StatusRequestEntityTooLarge)
				return
			}
			p.log.Error("a person's upload", "profile", s.Profile, "err", err)
			http.Error(w, "Couldn't store the file.", http.StatusInternalServerError)
			return
		}
		// Still theirs after the upload, which can take a while?
		ctx, turn, done, err := p.m.HumanInput(s.ID, holder)
		if err != nil {
			http.Error(w, "You no longer have control. The file is in the uploads.", http.StatusConflict)
			return
		}
		defer done()
		p.answer(ctx, w, turn, s, id, []string{f.Name})
		return
	}
}

// open reports whether chooser id is open in s's browser.
func (p *Picker) open(r *http.Request, s sessions.Session, id string) bool {
	cs, err := p.m.Choosers(r.Context(), s.Profile)
	if err != nil {
		return false
	}
	for _, c := range cs {
		if c.ID == id {
			return true
		}
	}
	return false
}

// limit is the most one upload may be: MaxUpload, or the files cap.
func (p *Picker) limit() int64 {
	if l := p.files.Limit(); l > 0 && l < p.max {
		return l
	}
	return p.max
}

// answer sends the browser the person's answer, for their takeover turn,
// with ctx (HumanInput's), and logs it without names.
func (p *Picker) answer(ctx context.Context, w http.ResponseWriter, turn uint64, s sessions.Session, id string, names []string) {
	err := p.m.AnswerChooser(ctx, s.Profile, id, names, turn)
	var why string
	switch {
	case errors.Is(err, sessions.ErrControlEnded), ctx.Err() != nil:
		// Handed back meanwhile: the browser, or this side, refused it.
		why = "You no longer have control."
		if len(names) > 0 {
			why += " The file stays in the uploads."
		}
		http.Error(w, why, http.StatusConflict)
		return
	case errors.Is(err, sessions.ErrChooserClosed):
		why = "That file chooser closed: the page closed it, or its time ran out."
		if len(names) > 0 {
			why += " The file stays in the uploads."
		}
		http.Error(w, why, http.StatusConflict)
		return
	case errors.Is(err, sessions.ErrNoPortal):
		http.Error(w, "This browser can't answer file choosers.", http.StatusServiceUnavailable)
		return
	case err != nil:
		// The browser's own refusal (several files to a single-file
		// chooser, say) or no answer.
		p.log.Warn("answering a file chooser for a person", "profile", s.Profile, "err", err)
		http.Error(w, "The browser refused that answer. One file only, unless the page takes several.", http.StatusUnprocessableEntity)
		return
	}
	what := cancelled
	if len(names) > 0 {
		what = chose(len(names))
	}
	p.m.NoteControl(s.ID, what)
	p.log.Info("a person answered a file chooser", "profile", s.Profile, "chooser", id, "files", len(names))
	w.WriteHeader(http.StatusNoContent)
}

// size is a file's size for people: KB under a megabyte, else MB.
func size(n int64) string {
	switch {
	case n < 1000:
		return fmt.Sprintf("%d bytes", n)
	case n < 1000*1000:
		return fmt.Sprintf("%d KB", (n+500)/1000)
	case n < 10*1000*1000:
		return fmt.Sprintf("%.1f MB", float64(n)/1e6)
	default:
		return fmt.Sprintf("%d MB", (n+500_000)/1_000_000)
	}
}

func (p *Picker) ago(t time.Time) string {
	d := p.now().Sub(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	default:
		return fmt.Sprintf("%d days ago", int(d.Hours()/24))
	}
}

// left is the time a chooser has before it closes, in whole minutes up.
func left(d time.Duration) string {
	if d < time.Minute {
		return "less than a minute"
	}
	return fmt.Sprintf("%d min", int(math.Ceil(d.Minutes())))
}
