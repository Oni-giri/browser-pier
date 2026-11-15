package pagetools

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/files"
)

func TestCheckStepUpload(t *testing.T) {
	ok := func(names ...string) Step { return Step{Action: "upload", Ref: "a7e1", Files: names} }
	for _, st := range []Step{ok("report.pdf"), ok("a b (2).zip", "Ünïcødé ✓.txt"), ok(strings.Repeat("a", 255)),
		ok("1", "2", "3", "4", "5", "6", "7", "8", "9", "10")} {
		if err := checkStep(st); err != nil {
			t.Errorf("%v: %v", st.Files, err)
		}
	}
	for _, st := range []Step{
		ok(), {Action: "upload", Ref: "a7e1"},
		ok("1", "2", "3", "4", "5", "6", "7", "8", "9", "10", "11"),
		ok("a/b"), ok("../etc/passwd"), ok(`a\b`), ok(".."), ok("a..b"), ok("nul\x00.pdf"), ok(""), ok(".hidden"),
		ok("tab\tname"), ok(strings.Repeat("a", 256)), ok("fine.pdf", "/etc/passwd"),
		{Action: "upload", Files: []string{"report.pdf"}}, // no ref
	} {
		if err := checkStep(st); err == nil {
			t.Errorf("%q accepted", st.Files)
		}
	}
}

// uploadPage is a page with file inputs: a plain one, a multiple one, a
// disabled one, a label controlling a hidden one, and a button.
type uploadPage struct {
	doc                                    *fnode
	single, multi, off, hidden, label, btn *fnode
}

func newUploadPage(fb *fakeBrowser) *uploadPage {
	p := &uploadPage{}
	input := func(name string, y float64) *fnode {
		n := el("input", "button", name, box(10, y, 200, 20))
		fb.behave[n] = &behaviour{field: "file"}
		return n
	}
	p.single = input("Attach", 100)
	p.multi = input("Attach several", 130)
	fb.behave[p.multi].multiple = true
	p.off = input("Attach nothing", 160)
	fb.behave[p.off].disabled = true
	p.hidden = el("input", "", "", box(10, 200, 1, 1))
	fb.behave[p.hidden] = &behaviour{field: "file"}
	p.label = el("label", "button", "Choose a photo", box(10, 200, 200, 30), p.hidden)
	fb.behave[p.label] = &behaviour{control: p.hidden}
	p.btn = el("button", "button", "Upload a file", box(10, 250, 100, 30))
	fb.behave[p.btn] = &behaviour{}
	p.doc = el("#document", "RootWebArea", "", box(0, 0, 1280, 800), p.single, p.multi, p.off, p.label, p.btn)
	return p
}

// uploads gives the fixture's tools a files store and writes files into
// the session's uploads folder.
func (f *fixture) uploads(names ...string) string {
	f.t.Helper()
	dir := filepath.Join(f.m.Layout().FilesDir(f.sess.Profile), files.Uploads)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		f.t.Fatal(err)
	}
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("content of "+n), 0o600); err != nil {
			f.t.Fatal(err)
		}
	}
	f.tools.SetFiles(files.New(f.m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()))
	return dir
}

// setFiles lists the paths of every DOM.setFileInputFiles the fake got.
func setFiles(t *testing.T, fb *fakeBrowser) [][]string {
	t.Helper()
	var out [][]string
	for _, m := range fb.Received() {
		if m.Method != "DOM.setFileInputFiles" {
			continue
		}
		var p struct {
			Files         []string `json:"files"`
			BackendNodeID int64    `json:"backendNodeId"`
			NodeID        int64    `json:"nodeId"`
			ObjectID      string   `json:"objectId"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			t.Fatal(err)
		}
		if p.BackendNodeID == 0 || p.NodeID != 0 || p.ObjectID != "" {
			t.Errorf("setFileInputFiles names its input by something other than its backend node: %s", m.Params)
		}
		for _, f := range p.Files {
			// Only ever a bare name under /files/uploads.
			if name, ok := strings.CutPrefix(f, "/files/uploads/"); !ok || !files.ValidName(name) || strings.Contains(name, "..") {
				t.Errorf("setFileInputFiles got %q", f)
			}
		}
		out = append(out, p.Files)
	}
	return out
}

func TestActUploads(t *testing.T) {
	f := newFixture(t)
	p := newUploadPage(f.fb)
	f.fb.addTab("https://example.com/upload", p.doc, true)
	f.uploads("report.pdf", "photo.jpg", "notes.txt")
	r := f.refs()

	out, err := f.act(Step{Action: "upload", Ref: r["Attach"], Files: []string{"report.pdf"}})
	if err != nil || !strings.Contains(out, "uploaded 1 file to "+r["Attach"]+` button "Attach"`) {
		t.Fatalf("upload: %v\n%s", err, out)
	}
	if got := f.fb.behave[p.single].files; len(got) != 1 || got[0] != "/files/uploads/report.pdf" {
		t.Errorf("the input holds %v", got)
	}
	// Several files to a multiple input.
	out, err = f.act(Step{Action: "upload", Ref: r["Attach several"], Files: []string{"photo.jpg", "notes.txt"}})
	if err != nil || !strings.Contains(out, "uploaded 2 files to "+r["Attach several"]) {
		t.Fatalf("upload two: %v\n%s", err, out)
	}
	if got := f.fb.behave[p.multi].files; strings.Join(got, ",") != "/files/uploads/photo.jpg,/files/uploads/notes.txt" {
		t.Errorf("the multiple input holds %v", got)
	}
	// Through a label: its control gets the file.
	out, err = f.act(Step{Action: "upload", Ref: r["Choose a photo"], Files: []string{"photo.jpg"}})
	if err != nil || !strings.Contains(out, "uploaded 1 file to "+r["Choose a photo"]) {
		t.Fatalf("upload through a label: %v\n%s", err, out)
	}
	if got := f.fb.behave[p.hidden].files; len(got) != 1 || got[0] != "/files/uploads/photo.jpg" {
		t.Errorf("the label's input holds %v", got)
	}
	// The activity log names the files and the ref.
	var logged []string
	for _, a := range f.m.Activity(f.sess.ID) {
		logged = append(logged, a.What)
	}
	for _, want := range []string{"Uploaded report.pdf to " + r["Attach"], "Uploaded photo.jpg, notes.txt to " + r["Attach several"]} {
		if !strings.Contains(strings.Join(logged, "\n"), want) {
			t.Errorf("activity has no %q: %q", want, logged)
		}
	}
	// No pointer or key event went out: nothing was clicked.
	for _, e := range f.fb.input(f.fb.tabs[0]).events {
		if !strings.HasPrefix(e, "setFileInputFiles") {
			t.Errorf("an input event went out: %s", e)
		}
	}
	if got := len(setFiles(t, f.fb)); got != 3 {
		t.Errorf("%d setFileInputFiles calls, want 3", got)
	}
	checkSent(t, f.fb)
}

func TestActUploadRefusals(t *testing.T) {
	f := newFixture(t)
	p := newUploadPage(f.fb)
	f.fb.addTab("https://example.com/upload", p.doc, true)
	dir := f.uploads("report.pdf", "photo.jpg")
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "link.txt")); err != nil {
		t.Fatal(err)
	}
	r := f.refs()
	for _, c := range []struct {
		name   string
		step   Step
		status int
		want   string
	}{
		{"several files to a single input", Step{Action: "upload", Ref: r["Attach"], Files: []string{"report.pdf", "photo.jpg"}}, 422, "takes one file"},
		{"a button", Step{Action: "upload", Ref: r["Upload a file"], Files: []string{"report.pdf"}}, 422, "file_upload handoff"},
		{"a disabled input", Step{Action: "upload", Ref: r["Attach nothing"], Files: []string{"report.pdf"}}, 422, "disabled"},
		{"a missing file", Step{Action: "upload", Ref: r["Attach"], Files: []string{"x"}}, 422, `no uploaded file named "x"; upload it first with upload_file`},
		{"a link out of the folder", Step{Action: "upload", Ref: r["Attach"], Files: []string{"link.txt"}}, 422, `no uploaded file named "link.txt"`},
		{"a path", Step{Action: "upload", Ref: r["Attach"], Files: []string{"../../secret.txt"}}, http.StatusBadRequest, "bare name"},
		{"no files", Step{Action: "upload", Ref: r["Attach"]}, http.StatusBadRequest, "1 to 10"},
	} {
		if _, err := f.act(c.step); !isStatus(err, c.status) || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v", c.name, err)
		}
	}
	// A button's refusal says what to do instead.
	_, err := f.act(Step{Action: "upload", Ref: r["Upload a file"], Files: []string{"report.pdf"}})
	for _, want := range []string{"<input type=file>", "styled out of sight", "<label>"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("the button's refusal lacks %q: %v", want, err)
		}
	}
	if got := setFiles(t, f.fb); len(got) != 0 {
		t.Errorf("refused uploads still set files: %v", got)
	}
	checkSent(t, f.fb)
}

// TestFileInputsPointAtTheUploadStep: clicking or filling a file input
// says to use upload instead.
func TestFileInputsPointAtTheUploadStep(t *testing.T) {
	f := newFixture(t)
	p := newActPage(f.fb)
	f.fb.addTab("https://example.com/", p.doc, true)
	r := f.refs()
	if _, err := f.act(Step{Action: "click", Ref: r["Upload"]}); !isStatus(err, 422) || !strings.Contains(err.Error(), "upload step") {
		t.Errorf("click: %v", err)
	}
	if _, err := f.act(Step{Action: "fill", Ref: r["Upload"], Text: text("x")}); !isStatus(err, 422) || !strings.Contains(err.Error(), "upload step") {
		t.Errorf("fill: %v", err)
	}
}

// TestNoUploadsWithoutAFilesStore: a Tools with no files store refuses.
func TestNoUploadsWithoutAFilesStore(t *testing.T) {
	f := newFixture(t)
	p := newUploadPage(f.fb)
	f.fb.addTab("https://example.com/upload", p.doc, true)
	r := f.refs()
	if _, err := f.act(Step{Action: "upload", Ref: r["Attach"], Files: []string{"report.pdf"}}); !isStatus(err, 422) {
		t.Errorf("got %v", err)
	}
}
