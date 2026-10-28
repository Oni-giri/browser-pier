package ui

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/archive"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// maxImport bounds an uploaded archive.
const maxImport = 8 << 30

// exportProfile streams a stopped profile as an encrypted archive. The
// archive holds live session cookies: it's for the admin only, never the
// agent API.
func (u *UI) exportProfile(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	p, err := u.m.Profiles().Get(r.Context(), name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	pass := r.PostFormValue("passphrase")
	switch {
	case pass != r.PostFormValue("confirm"):
		done(w, r, "/profiles", "The passphrases don't match.", true)
		return
	case len(pass) < archive.MinPassphrase:
		done(w, r, "/profiles", "Use a passphrase of at least 10 characters.", true)
		return
	}
	release, err := u.m.Hold(name)
	switch {
	case errors.Is(err, sessions.ErrProfileBusy):
		done(w, r, "/profiles", "Stop "+name+"'s browser before exporting it.", true)
		return
	case err != nil:
		done(w, r, "/profiles", "Couldn't export "+name+": "+err.Error(), true)
		return
	}
	defer release()
	m := archive.Manifest{Version: u.o.Version, ExportedAt: time.Now().UTC(), Profile: archive.Profile{
		Name: p.Name, DefaultMode: string(p.DefaultMode), AutoStopMinutes: int(p.AutoStop / time.Minute),
		Width: p.Width, Height: p.Height, Proxy: p.Proxy, HandoffPolicy: string(p.HandoffPolicy),
		TimeZone: p.TimeZone, Locale: p.Locale,
	}}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+"-"+time.Now().Format("2006-01-02")+`.browserpier.age"`)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	u.log.Info("exporting a profile", "profile", name)
	if err := archive.Export(w, u.m.Layout().ProfileDir(name), m, pass); err != nil {
		u.log.Error("exporting a profile", "profile", name, "err", err)
		// The download has begun: cut it, so it can't pass for a whole one.
		panic(http.ErrAbortHandler)
	}
}

// importProfile reads an uploaded archive into a new profile. The form's
// file comes last, so it is streamed from the request, never stored whole.
func (u *UI) importProfile(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)
	mr, err := r.MultipartReader()
	if err != nil {
		done(w, r, "/profiles", "Choose an archive to import.", true)
		return
	}
	var name, pass string
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			done(w, r, "/profiles", "Choose an archive to import.", true)
			return
		}
		if err != nil {
			done(w, r, "/profiles", "The upload was cut short.", true)
			return
		}
		switch part.FormName() {
		case "name":
			name = strings.TrimSpace(formValue(part))
		case "passphrase":
			pass = formValue(part)
		case "archive":
			created, msg := u.importArchive(part, name, pass)
			_ = part.Close()
			if msg != "" {
				done(w, r, "/profiles", msg, true)
				return
			}
			u.forget()
			done(w, r, "/profiles/"+url.PathEscape(created), "Imported "+created, false)
			return
		}
		_ = part.Close()
	}
}

func formValue(r io.Reader) string {
	b, _ := io.ReadAll(io.LimitReader(r, 4<<10))
	return string(b)
}

// importArchive extracts into a folder of its own, then makes it the
// profile's. It returns the new profile's name, or what went wrong.
func (u *UI) importArchive(r io.Reader, name, pass string) (string, string) {
	layout := u.m.Layout()
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	tmp := filepath.Join(layout.DataDir, "profiles", sessions.ImportPrefix+hex.EncodeToString(b))
	if err := os.MkdirAll(filepath.Dir(tmp), 0o700); err != nil {
		return "", "Couldn't import: " + err.Error()
	}
	lim := archive.DefaultLimits
	lim.UID, lim.GID, lim.Chown = layout.UID, layout.GID, os.Getuid() == 0
	m, err := archive.Import(r, pass, tmp, lim)
	if err != nil {
		_ = os.RemoveAll(tmp)
		u.log.Warn("importing a profile", "err", err)
		return "", "Couldn't import: " + err.Error()
	}
	if name == "" {
		name = m.Profile.Name
	}
	p := profiles.New(name)
	p.DefaultMode = profiles.Mode(m.Profile.DefaultMode)
	p.AutoStop = time.Duration(m.Profile.AutoStopMinutes) * time.Minute
	p.Width, p.Height = m.Profile.Width, m.Profile.Height
	p.Proxy, p.HandoffPolicy = m.Profile.Proxy, profiles.HandoffPolicy(m.Profile.HandoffPolicy)
	p.TimeZone, p.Locale = m.Profile.TimeZone, m.Profile.Locale
	if err := p.Validate(); err != nil {
		_ = os.RemoveAll(tmp)
		return "", "Couldn't import: " + sentence(err.Error())
	}
	release, err := u.m.Hold(name)
	if err != nil {
		_ = os.RemoveAll(tmp)
		return "", "Couldn't import as " + name + ": " + err.Error()
	}
	defer release()
	if _, err := u.m.Profiles().Create(context.Background(), p); err != nil {
		_ = os.RemoveAll(tmp)
		if errors.Is(err, profiles.ErrExists) {
			return "", "There's already a profile called " + name + ". Choose another name."
		}
		return "", "Couldn't import: " + err.Error()
	}
	// The folder becomes the profile's last, so a failure before leaves
	// no profile without its logins.
	if err := os.Rename(tmp, layout.ProfileDir(name)); err != nil {
		_ = os.RemoveAll(tmp)
		_ = u.m.Profiles().Delete(context.Background(), name)
		return "", "Couldn't import: " + err.Error()
	}
	u.log.Info("profile imported from the UI", "profile", name, "exported_at", m.ExportedAt)
	return name, ""
}
