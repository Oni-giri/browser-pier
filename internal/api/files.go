package api

import (
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
)

// maxUpload is the largest file one upload may carry.
const maxUpload = 100 << 20

// profileFor loads the {name} profile if the token may see it, or answers 404.
func (a *API) profileFor(w http.ResponseWriter, r *http.Request) (string, bool) {
	tok, _ := auth.TokenFromContext(r.Context())
	name := r.PathValue("name")
	if !tok.Allows(name) {
		writeError(w, http.StatusNotFound, "no profile named "+name)
		return "", false
	}
	if _, err := a.m.Profiles().Get(r.Context(), name); err != nil {
		if errors.Is(err, profiles.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no profile named "+name)
		} else {
			a.internal(w, "loading a profile", err)
		}
		return "", false
	}
	return name, true
}

func (a *API) listFiles(w http.ResponseWriter, r *http.Request) {
	name, ok := a.profileFor(w, r)
	if !ok {
		return
	}
	list, err := a.files.List(name)
	if err != nil {
		a.internal(w, "listing files", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": list, "limit_bytes": a.files.Limit()})
}

// getFile sends a download. Downloaded files come from untrusted pages, so
// they are always sent as attachments, never rendered.
func (a *API) getFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.profileFor(w, r)
	if !ok {
		return
	}
	f, info, err := a.files.Open(name, r.PathValue("file"))
	if errors.Is(err, files.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	if err != nil {
		a.internal(w, "opening a file", err)
		return
	}
	defer func() { _ = f.Close() }()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "sandbox")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": info.Name}))
	http.ServeContent(w, r, "", info.Modified, f)
}

func (a *API) deleteFile(w http.ResponseWriter, r *http.Request) {
	name, ok := a.profileFor(w, r)
	if !ok {
		return
	}
	err := a.files.Delete(name, r.PathValue("file"))
	if errors.Is(err, files.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such file")
		return
	}
	if err != nil {
		a.internal(w, "deleting a file", err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// uploadFile puts a file in the session's /files/uploads, where an agent
// can set it on a file input (DOM.setFileInputFiles) and a person can pick
// it in the file chooser. It takes a multipart form with a "file" field,
// or the raw body with ?name=.
func (a *API) uploadFile(w http.ResponseWriter, r *http.Request) {
	s, ok := a.running(w, r, false) // a person may need the file during a handoff
	if !ok {
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload+1<<20)
	var body io.Reader = r.Body
	name := r.URL.Query().Get("name")
	if mt, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type")); mt == "multipart/form-data" {
		mr, err := r.MultipartReader()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad multipart body")
			return
		}
		for {
			part, err := mr.NextPart()
			if err != nil {
				writeError(w, http.StatusBadRequest, `send the file in a form field named "file"`)
				return
			}
			if part.FormName() == "file" {
				body = part
				if name == "" {
					name = part.FileName()
				}
				break
			}
		}
	}
	if name == "" {
		writeError(w, http.StatusBadRequest, "give the file a name: a multipart filename, or ?name=")
		return
	}
	a.m.Touch(s.ID)
	f, path, err := a.files.Upload(s.Profile, name, body, maxUpload)
	switch {
	case errors.Is(err, files.ErrBadName):
		writeError(w, http.StatusBadRequest, "name: use a plain file name, without folders, not starting with a dot")
		return
	case errors.Is(err, files.ErrTooLarge):
		writeError(w, http.StatusRequestEntityTooLarge, "the file is over 100 MiB or the profile's files cap")
		return
	case errors.Is(err, files.ErrOverCap):
		writeError(w, http.StatusInsufficientStorage, err.Error())
		return
	case err != nil:
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeError(w, http.StatusRequestEntityTooLarge, "the file is over 100 MiB")
			return
		}
		a.internal(w, "uploading a file", err)
		return
	}
	tok, _ := auth.TokenFromContext(r.Context())
	a.log.Info("file uploaded", "profile", s.Profile, "file", f.Name, "bytes", f.Size, "token", tok.Name)
	writeJSON(w, http.StatusCreated, map[string]any{"name": f.Name, "size": f.Size, "path": path,
		"url": a.baseURL(r) + "/api/profiles/" + url.PathEscape(s.Profile) + "/files"})
}
