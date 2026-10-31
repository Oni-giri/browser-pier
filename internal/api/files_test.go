package api

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rawReply is a finished response, body read and closed.
type rawReply struct {
	StatusCode int
	Header     http.Header
}

func (f *fixture) raw(token, method, path string, body io.Reader, contentType string) (rawReply, string) {
	f.t.Helper()
	req, _ := http.NewRequest(method, f.srv.URL+path, body)
	req.Header.Set("Authorization", "Bearer "+token)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		f.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return rawReply{resp.StatusCode, resp.Header}, string(b)
}

func TestDownloads(t *testing.T) {
	f := newFixture(t)
	dl := filepath.Join(f.m.Layout().FilesDir("google"), "downloads")
	if err := os.MkdirAll(dl, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dl, "report.html"), []byte("<script>alert(1)</script>"), 0o600); err != nil {
		t.Fatal(err)
	}

	var list struct {
		Files      []struct{ Name string }
		LimitBytes int64 `json:"limit_bytes"`
	}
	if code := f.call(f.all, http.MethodGet, "/api/profiles/google/files", "", &list); code != http.StatusOK || len(list.Files) != 1 || list.Files[0].Name != "report.html" || list.LimitBytes != 1<<20 {
		t.Fatalf("list: %d %+v", code, list)
	}
	if code := f.call(f.all, http.MethodGet, "/api/profiles/github/files", "", &list); code != http.StatusOK || len(list.Files) != 0 {
		t.Fatalf("a profile with no files: %d %+v", code, list)
	}
	if code := f.call(f.scoped, http.MethodGet, "/api/profiles/github/files", "", nil); code != http.StatusNotFound {
		t.Fatalf("scoped token on another profile: %d", code)
	}
	if code := f.call(f.all, http.MethodGet, "/api/profiles/nope/files", "", nil); code != http.StatusNotFound {
		t.Fatalf("unknown profile: %d", code)
	}

	resp, body := f.raw(f.all, http.MethodGet, "/api/profiles/google/files/report.html", nil, "")
	if resp.StatusCode != http.StatusOK || body != "<script>alert(1)</script>" {
		t.Fatalf("fetch: %d %q", resp.StatusCode, body)
	}
	// A page's download is never rendered as HTML here.
	if resp.Header.Get("Content-Type") != "application/octet-stream" || resp.Header.Get("X-Content-Type-Options") != "nosniff" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("headers %v", resp.Header)
	}
	if resp, _ := f.raw(f.all, http.MethodGet, "/api/profiles/google/files/..%2Fchrome%2FCookies", nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("path traversal: %d", resp.StatusCode)
	}
	if resp, _ := f.raw(f.all, http.MethodDelete, "/api/profiles/google/files/report.html", nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	if resp, _ := f.raw(f.all, http.MethodGet, "/api/profiles/google/files/report.html", nil, ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("fetch after delete: %d", resp.StatusCode)
	}
}

func TestUploads(t *testing.T) {
	f := newFixture(t)
	s := f.start(f.all, "google", "visual")

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "cv.pdf")
	_, _ = part.Write([]byte("%PDF-1.7"))
	_ = mw.Close()
	resp, body := f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/files", &buf, mw.FormDataContentType())
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"path":"/files/uploads/cv.pdf"`) {
		t.Fatalf("multipart upload: %d %s", resp.StatusCode, body)
	}
	got, err := os.ReadFile(filepath.Join(f.m.Layout().FilesDir("google"), "uploads", "cv.pdf"))
	if err != nil || string(got) != "%PDF-1.7" {
		t.Fatalf("uploaded file: %q, %v", got, err)
	}

	resp, body = f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/files?name=notes.txt", strings.NewReader("hello"), "application/octet-stream")
	if resp.StatusCode != http.StatusCreated || !strings.Contains(body, `"path":"/files/uploads/notes.txt"`) {
		t.Fatalf("raw upload: %d %s", resp.StatusCode, body)
	}
	for _, bad := range []string{"", "../x", ".bashrc"} {
		if resp, _ := f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/files?name="+bad, strings.NewReader("x"), ""); resp.StatusCode != http.StatusBadRequest {
			t.Errorf("name %q: %d", bad, resp.StatusCode)
		}
	}
	if resp, _ := f.raw(f.scoped, http.MethodPost, "/api/sessions/"+f.start(f.all, "github", "").ID+"/files?name=x", strings.NewReader("x"), ""); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("scoped token uploading to another profile: %d", resp.StatusCode)
	}
	if _, err := f.m.Stop(t.Context(), s.ID); err != nil {
		t.Fatal(err)
	}
	if resp, _ := f.raw(f.all, http.MethodPost, "/api/sessions/"+s.ID+"/files?name=late.txt", strings.NewReader("x"), ""); resp.StatusCode != http.StatusConflict {
		t.Fatalf("upload to a stopped session: %d", resp.StatusCode)
	}
}
