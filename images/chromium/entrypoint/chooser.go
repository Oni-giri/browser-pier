package main

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/godbus/dbus/v5"
)

// The file chooser portal's requests (PROJ-152), apart from D-Bus
// (portal.go): what Chromium asked, what may answer it, and how long it
// waits. A page's file chooser (an <input type=file>, a button calling
// its click(), showOpenFilePicker) is held here until the agent or the
// person answers it with files from /files/uploads, cancels it, Chromium
// closes it (the tab went away), Chromium exits, or the time limit
// passes. A "Save as" (ctrl+S, showSaveFilePicker) is answered at once
// with a new file in /files/downloads.
//
// No path from outside reaches an answer: the control socket sends bare
// names, each must be a regular file directly in the uploads folder
// (lstat, through os.Root), and the URI is built here.

const (
	// filesDir is where the orchestrator mounts the profile's files.
	filesDir = "/files"
	// maxOpenChoosers is how many choosers may wait at once; Chromium
	// asks for one per window at a time, so more is a page at play.
	maxOpenChoosers = 4
	// maxChosen is the most files one answer may give.
	maxChosen = 10
	// maxPatterns and maxPatternLen cap what a chooser reports of the
	// page's accept list.
	maxPatterns   = 32
	maxPatternLen = 64
	// maxSaves is how many "Save as" answers may be given within
	// saveWindow; past that one is refused.
	maxSaves   = 64
	saveWindow = 10 * time.Minute
	// maxSaveName caps a saved file's name, in bytes.
	maxSaveName = 240
	// saveDelay is how long a save's answer waits, so that it follows
	// the method's reply on the bus.
	saveDelay = 50 * time.Millisecond
)

// Response codes of org.freedesktop.portal.Request.Response.
const (
	respOK        uint32 = 0
	respCancelled uint32 = 1
)

// chooserReq is one waiting OpenFile.
type chooserReq struct {
	id       string
	path     dbus.ObjectPath
	sender   string
	multiple bool
	accept   []string
	opened   time.Time
	timer    stopper
	deadline time.Time // when the timer cancels it
}

// stopper is a timer as the clock gives it.
type stopper interface{ Stop() bool }

// portalBus is what the chooser needs from D-Bus: answering a request,
// and the request objects Chromium may call Close on. portal.go has the
// real one; the tests a fake.
type portalBus interface {
	respond(sender string, path dbus.ObjectPath, code uint32, results map[string]dbus.Variant) error
	exportRequest(path dbus.ObjectPath, close func(sender string) *dbus.Error) error
	unexportRequest(path dbus.ObjectPath)
}

// portalRefused is the D-Bus error for a request the portal won't serve.
func portalRefused(why string) *dbus.Error {
	return dbus.NewError("org.freedesktop.portal.Error.NotAllowed", []any{why})
}

// chooser holds the requests. Its zero value isn't usable: see newChooser.
type chooser struct {
	log       *slog.Logger
	bus       portalBus
	check     func(sender string) error // the caller check (caller.go)
	focus     focuser
	timeout   time.Duration
	now       func() time.Time
	after     func(time.Duration, func()) stopper
	uploads   string
	downloads string

	mu    sync.Mutex
	next  int // the last id given; ids are never reused
	open  map[string]*chooserReq
	saves []time.Time
}

func newChooser(log *slog.Logger, bus portalBus, check func(string) error, focus focuser, timeout time.Duration) *chooser {
	return &chooser{
		log: log, bus: bus, check: check, focus: focus, timeout: timeout,
		now:     time.Now,
		after:   func(d time.Duration, f func()) stopper { return time.AfterFunc(d, f) },
		uploads: path.Join(filesDir, "uploads"), downloads: path.Join(filesDir, "downloads"),
		open: map[string]*chooserReq{},
	}
}

// requestPath is the request object's path the portal API sets: the
// sender's unique name without its colon and with _ for each dot, and
// the caller's token, which must be a D-Bus name element. Without one,
// the request's own id stands in.
func requestPath(sender string, opts map[string]dbus.Variant, id string) dbus.ObjectPath {
	tok := id
	if v, ok := opts["handle_token"]; ok {
		if s, ok := v.Value().(string); ok && validToken(s) {
			tok = s
		}
	}
	s := strings.ReplaceAll(strings.TrimPrefix(sender, ":"), ".", "_")
	return dbus.ObjectPath("/org/freedesktop/portal/desktop/request/" + s + "/" + tok)
}

var tokenRE = regexp.MustCompile(`^[A-Za-z0-9_]{1,64}$`)

func validToken(s string) bool { return tokenRE.MatchString(s) }

func optBool(opts map[string]dbus.Variant, key string) bool {
	v, ok := opts[key]
	if !ok {
		return false
	}
	b, _ := v.Value().(bool)
	return b
}

// openFile holds a page's chooser until it is answered. A directory
// chooser is refused, as is one over maxOpenChoosers.
func (c *chooser) openFile(sender string, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	if err := c.check(sender); err != nil {
		c.log.Warn("refused a file chooser from a process that isn't the browser", "err", err)
		return "", portalRefused("not the browser")
	}
	if optBool(opts, "directory") {
		return "", portalRefused("folders can't be chosen")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.open) >= maxOpenChoosers {
		return "", portalRefused("too many file choosers open")
	}
	c.next++
	id := "c" + strconv.Itoa(c.next)
	r := &chooserReq{
		id: id, sender: sender, path: requestPath(sender, opts, id),
		multiple: optBool(opts, "multiple"), accept: acceptPatterns(opts), opened: c.now(),
	}
	for _, o := range c.open {
		if o.path == r.path {
			return "", portalRefused("that request is already open")
		}
	}
	if err := c.bus.exportRequest(r.path, func(from string) *dbus.Error { return c.closeReq(r, from) }); err != nil {
		return "", dbus.MakeFailedError(err)
	}
	c.open[id] = r
	c.startTimerLocked(r)
	if len(c.open) == 1 {
		c.focus.away()
	}
	c.log.Info("a page opened a file chooser", "id", id, "multiple", r.multiple)
	return r.path, nil
}

func (c *chooser) startTimerLocked(r *chooserReq) {
	r.deadline = c.now().Add(c.timeout)
	r.timer = c.after(c.timeout, func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		// Gone, or restarted while this timer was waiting for the lock:
		// a new timer has its own deadline.
		if c.open[r.id] != r || c.now().Before(r.deadline) {
			return
		}
		c.log.Info("a file chooser was cancelled: nobody answered it", "id", r.id, "after", c.timeout)
		c.endLocked(r, respCancelled, map[string]dbus.Variant{})
	})
}

// endLocked removes r, answering it with code unless code is nil (Chromium
// closed it, or is gone).
func (c *chooser) endLocked(r *chooserReq, code uint32, results map[string]dbus.Variant) {
	c.dropLocked(r)
	if results != nil {
		if err := c.bus.respond(r.sender, r.path, code, results); err != nil {
			c.log.Warn("couldn't answer a file chooser", "id", r.id, "err", err)
		}
	}
}

func (c *chooser) dropLocked(r *chooserReq) {
	delete(c.open, r.id)
	if r.timer != nil {
		r.timer.Stop()
	}
	c.bus.unexportRequest(r.path)
	if len(c.open) == 0 {
		c.focus.back()
	}
}

// closeReq is Chromium's Close on a request: the tab went away or
// another took the front. Only the caller that asked may close it.
func (c *chooser) closeReq(r *chooserReq, from string) *dbus.Error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.open[r.id] != r {
		return nil
	}
	if from != r.sender {
		return portalRefused("not the caller that asked")
	}
	c.log.Info("the browser closed a file chooser", "id", r.id)
	c.dropLocked(r)
	return nil
}

// gone ends the requests sender made, without answering: it has left
// the bus.
func (c *chooser) gone(sender string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.open {
		if r.sender == sender {
			c.dropLocked(r)
		}
	}
}

// browserExited ends every request: the browser that asked is gone.
func (c *chooser) browserExited() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.open {
		c.dropLocked(r)
	}
}

// restart gives every open chooser its whole time limit again: control
// of the browser changed hands (PROJ-181), and whoever has it now gets
// the time to answer.
func (c *chooser) restart() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.open {
		if r.timer != nil {
			r.timer.Stop()
		}
		c.startTimerLocked(r)
	}
	return len(c.open)
}

// chooserInfo is one open chooser as the control socket lists it.
type chooserInfo struct {
	ID       string   `json:"id"`
	Multiple bool     `json:"multiple"`
	Accept   []string `json:"accept"`
	AgeMS    int64    `json:"age_ms"`
	// LeftMS is how long until the time limit cancels it.
	LeftMS int64 `json:"left_ms"`
}

// list is the open choosers, oldest first.
func (c *chooser) list() []chooserInfo {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]chooserInfo, 0, len(c.open))
	now := c.now()
	for _, r := range c.open {
		out = append(out, chooserInfo{ID: r.id, Multiple: r.multiple, Accept: append([]string{}, r.accept...), AgeMS: now.Sub(r.opened).Milliseconds(),
			LeftMS: max(0, r.deadline.Sub(now).Milliseconds())})
	}
	sortChoosers(out)
	return out
}

func sortChoosers(cs []chooserInfo) {
	n := func(id string) int { v, _ := strconv.Atoi(strings.TrimPrefix(id, "c")); return v }
	slices.SortFunc(cs, func(a, b chooserInfo) int { return n(a.ID) - n(b.ID) })
}

// answerError is a refused answer, with the HTTP status the control
// socket gives it.
type answerError struct {
	status int
	msg    string
}

func (e *answerError) Error() string { return e.msg }

var errNoChooser = &answerError{404, "no open file chooser with that id"}

// answer gives chooser id the files named, all from the uploads folder.
// Nothing is sent to the browser unless every name passes.
func (c *chooser) answer(id string, names []string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.open[id]
	if r == nil {
		return errNoChooser
	}
	uris, err := c.uploadURIs(names, r.multiple)
	if err != nil {
		return err
	}
	c.log.Info("answered a file chooser", "id", id, "files", len(uris))
	c.endLocked(r, respOK, map[string]dbus.Variant{"uris": dbus.MakeVariant(uris)})
	return nil
}

// cancel answers chooser id with no files.
func (c *chooser) cancel(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.open[id]
	if r == nil {
		return errNoChooser
	}
	c.log.Info("cancelled a file chooser", "id", id)
	c.endLocked(r, respCancelled, map[string]dbus.Variant{})
	return nil
}

// uploadURIs checks names and builds their URIs: one name unless
// multiple, at most maxChosen, each a plain name of a regular file
// directly in the uploads folder (not a link, a folder or anything
// else), each URI exactly file:///files/uploads/<percent-encoded name>.
func (c *chooser) uploadURIs(names []string, multiple bool) ([]string, error) {
	switch {
	case len(names) == 0:
		return nil, &answerError{400, "no files: send cancel instead"}
	case len(names) > maxChosen:
		return nil, &answerError{422, fmt.Sprintf("at most %d files at once", maxChosen)}
	case len(names) > 1 && !multiple:
		return nil, &answerError{422, "this file chooser takes one file"}
	}
	// The folder itself must be a real one: a link there (to /profile,
	// say) would make its files choosable.
	if fi, err := os.Lstat(c.uploads); err != nil || !fi.IsDir() {
		return nil, &answerError{422, "no uploaded files"}
	}
	root, err := os.OpenRoot(c.uploads)
	if err != nil {
		return nil, &answerError{422, "no uploaded files"}
	}
	defer func() { _ = root.Close() }()
	uris := make([]string, 0, len(names))
	for _, name := range names {
		if !plainName(name) {
			return nil, &answerError{400, "file names: plain names of uploaded files, no folders, not starting with a dot"}
		}
		fi, err := root.Lstat(name)
		if err != nil || !fi.Mode().IsRegular() {
			return nil, &answerError{422, "no uploaded file named " + strconv.Quote(name)}
		}
		uris = append(uris, "file://"+c.uploads+"/"+url.PathEscape(name))
	}
	return uris, nil
}

// plainName is files.ValidName's rule: no folders, not hidden,
// printable, at most 255 bytes.
func plainName(name string) bool {
	if name == "" || len(name) > 255 || !utf8.ValidString(name) || strings.HasPrefix(name, ".") {
		return false
	}
	for _, r := range name {
		if r == '/' || r == '\\' || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// saveFile answers a "Save as" at once with a new, empty file in the
// downloads folder, named after the suggestion.
func (c *chooser) saveFile(sender string, opts map[string]dbus.Variant) (dbus.ObjectPath, *dbus.Error) {
	if err := c.check(sender); err != nil {
		c.log.Warn("refused a save from a process that isn't the browser", "err", err)
		return "", portalRefused("not the browser")
	}
	c.mu.Lock()
	now := c.now()
	kept := c.saves[:0]
	for _, t := range c.saves {
		if now.Sub(t) < saveWindow {
			kept = append(kept, t)
		}
	}
	c.saves = kept
	if len(c.saves) >= maxSaves {
		c.mu.Unlock()
		return "", portalRefused("too many saves")
	}
	c.saves = append(c.saves, now)
	c.next++
	id := "s" + strconv.Itoa(c.next)
	c.mu.Unlock()

	suggested, _ := opts["current_name"].Value().(string)
	name, err := createSaveFile(c.downloads, suggested)
	if err != nil {
		c.log.Warn("couldn't answer a save", "err", err)
		return "", dbus.MakeFailedError(errors.New("couldn't create the file"))
	}
	p := requestPath(sender, opts, id)
	uri := "file://" + c.downloads + "/" + url.PathEscape(name)
	c.log.Info("answered a save with a new file in the downloads", "id", id)
	c.after(saveDelay, func() {
		if err := c.bus.respond(sender, p, respOK, map[string]dbus.Variant{"uris": dbus.MakeVariant([]string{uri})}); err != nil {
			c.log.Warn("couldn't answer a save", "id", id, "err", err)
		}
	})
	return p, nil
}

// saveName cleans a page's suggested name: no path separators or
// control characters, no leading dot or space, at most maxSaveName
// bytes (the extension kept), "download" if nothing is left.
func saveName(suggested string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(suggested, "_") {
		switch {
		case r == '/' || r == '\\':
			b.WriteRune('_')
		case !unicode.IsPrint(r):
		default:
			b.WriteRune(r)
		}
	}
	name := strings.TrimLeft(strings.TrimSpace(b.String()), ". ")
	if name == "" {
		name = "download"
	}
	if len(name) > maxSaveName {
		ext := path.Ext(name)
		if len(ext) > 16 {
			ext = ""
		}
		stem := name[:len(name)-len(ext)]
		for len(stem)+len(ext) > maxSaveName {
			_, n := utf8.DecodeLastRuneInString(stem)
			stem = stem[:len(stem)-n]
		}
		name = stem + ext
	}
	return name
}

// createSaveFile creates the save's file in dir, exclusively and without
// following a link: name, or name (2), (3)… before its extension when
// taken. A link planted under the name counts as taken.
func createSaveFile(dir, suggested string) (string, error) {
	if fi, err := os.Lstat(dir); err != nil || !fi.IsDir() {
		return "", errors.New("the downloads folder isn't a folder")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer func() { _ = root.Close() }()
	name := saveName(suggested)
	ext := path.Ext(name)
	stem := strings.TrimSuffix(name, ext)
	for i := 1; i < 1000; i++ {
		try := name
		if i > 1 {
			try = fmt.Sprintf("%s (%d)%s", stem, i, ext)
			for len(try) > 255 {
				_, n := utf8.DecodeLastRuneInString(stem)
				stem = stem[:len(stem)-n]
				try = fmt.Sprintf("%s (%d)%s", stem, i, ext)
			}
		}
		f, err := root.OpenFile(try, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o644)
		if err == nil {
			return try, f.Close()
		}
		if !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	return "", errors.New("too many files with that name")
}

// acceptPatterns is what a chooser reports of the page's accept list:
// the glob patterns of its filters (Chromium turns each MIME type into
// its extensions, written case-blind as *.[pP][dD][fF]: *.pdf here), not
// the catch-all, at most maxPatterns of at most maxPatternLen bytes,
// printable only. They come from the page: the orchestrator fences them.
func acceptPatterns(opts map[string]dbus.Variant) []string {
	out := []string{}
	seen := map[string]bool{}
	add := func(p string) {
		p = caseBlind.ReplaceAllStringFunc(p, func(m string) string {
			sm := caseBlind.FindStringSubmatch(m)
			if sm[1] != sm[2] && strings.EqualFold(sm[1], sm[2]) {
				return strings.ToLower(sm[1])
			}
			return m
		})
		p = strings.Map(func(r rune) rune {
			if !unicode.IsPrint(r) {
				return -1
			}
			return r
		}, strings.ToValidUTF8(p, ""))
		if p == "" || p == "*" || p == "*.*" || len(p) > maxPatternLen || seen[p] || len(out) >= maxPatterns {
			return
		}
		seen[p] = true
		out = append(out, p)
	}
	// filters is a(sa(us)) and current_filter (sa(us)): each filter a
	// name (the page's, for showOpenFilePicker: never reported) and
	// its (kind, pattern) pairs.
	for _, key := range []string{"filters", "current_filter"} {
		v, ok := opts[key]
		if !ok {
			continue
		}
		list := []reflect.Value{reflect.ValueOf(v.Value())}
		if key == "filters" {
			list = seq(list[0])
		}
		for _, f := range list {
			nf := seq(f)
			if len(nf) != 2 {
				continue
			}
			for _, pt := range seq(nf[1]) {
				kp := seq(pt)
				if len(kp) != 2 {
					continue
				}
				if s, ok := kp[1].Interface().(string); ok {
					add(s)
				}
			}
		}
	}
	return out
}

var caseBlind = regexp.MustCompile(`\[(.)(.)\]`)

// seq is the elements of a D-Bus array or struct as godbus gives it
// (a slice, or a struct when built here), through interfaces and
// variants; nothing for anything else.
func seq(v reflect.Value) []reflect.Value {
	for v.IsValid() && v.Kind() == reflect.Interface {
		v = v.Elem()
	}
	if !v.IsValid() {
		return nil
	}
	if dv, ok := v.Interface().(dbus.Variant); ok {
		return seq(reflect.ValueOf(dv.Value()))
	}
	var out []reflect.Value
	switch v.Kind() {
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return nil
		}
		for i := range min(v.Len(), 256) {
			e := v.Index(i)
			for e.Kind() == reflect.Interface {
				e = e.Elem()
			}
			out = append(out, e)
		}
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				out = append(out, v.Field(i))
			}
		}
	}
	return out
}
