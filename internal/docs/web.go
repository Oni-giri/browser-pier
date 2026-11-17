package docs

import (
	"crypto/sha256"
	"encoding/base64"
	"html/template"
	"io/fs"
	"net/http"
	"path"
	"regexp"
	"strings"
)

// The docs' Content-Security-Policy: this server's files only, as the web
// UI's. Scripts are this server's or one of the page's own inline scripts,
// allowed by hash, never any inline script; WebAssembly is for the search
// (Pagefind), styles may be inline (Starlight's and Expressive Code's).
const cspBase = "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; font-src 'self'; " +
	"connect-src 'self'; worker-src 'self'; object-src 'none'; form-action 'none'; frame-ancestors 'none'; base-uri 'none'"

const scriptBase = "script-src 'self' 'wasm-unsafe-eval'"

var inlineScript = regexp.MustCompile(`(?s)<script(\s[^>]*)?>(.*?)</script>`)

// htmlCSP is the policy for one HTML file: its inline scripts by hash.
func htmlCSP(html string) string {
	var b strings.Builder
	b.WriteString(scriptBase)
	seen := map[string]bool{}
	for _, m := range inlineScript.FindAllStringSubmatch(html, -1) {
		if strings.Contains(m[1], "src=") {
			continue
		}
		sum := sha256.Sum256([]byte(m[2]))
		h := "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
		if !seen[h] {
			seen[h] = true
			b.WriteString(" " + h)
		}
	}
	return cspBase + "; " + b.String()
}

// Handler serves the site, with the /docs prefix already stripped. Who
// may read it is the caller's to check.
func (d *Docs) Handler() http.Handler {
	files := http.FileServerFS(d.fsys)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "same-origin")
		if !d.Built() {
			d.placeholder(w, r)
			return
		}
		name := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
		if name == "" {
			name = "."
		}
		info, err := fs.Stat(d.fsys, name)
		if err != nil || (strings.HasPrefix(name, ".") && name != ".") || strings.Contains(name, "/.") {
			d.notFound(w)
			return
		}
		file := name
		if info.IsDir() {
			file = path.Join(name, "index.html")
			if _, err := fs.Stat(d.fsys, file); err != nil {
				d.notFound(w)
				return
			}
		}
		switch {
		case path.Ext(file) == ".html":
			h.Set("Content-Security-Policy", d.csp[file])
		default:
			h.Set("Content-Security-Policy", cspBase+"; "+scriptBase)
		}
		switch path.Ext(file) {
		case ".md":
			h.Set("Content-Type", "text/markdown; charset=utf-8")
		case ".txt":
			h.Set("Content-Type", "text/plain; charset=utf-8")
		}
		// Built assets have the content's hash in their names; everything
		// else is checked again, as a new version may have changed it.
		if strings.HasPrefix(name, "_astro/") {
			h.Set("Cache-Control", "private, max-age=31536000, immutable")
		} else {
			h.Set("Cache-Control", "private, no-cache")
		}
		files.ServeHTTP(w, r)
	})
}

func (d *Docs) notFound(w http.ResponseWriter) {
	b, err := fs.ReadFile(d.fsys, "404.html")
	if err != nil {
		http.NotFound(w, nil)
		return
	}
	w.Header().Set("Content-Security-Policy", d.csp["404.html"])
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write(b)
}

var placeholderPage = template.Must(template.New("").Parse(`<!doctype html>
<html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<title>BrowserPier docs</title>
<style>body{font:16px/1.6 system-ui,sans-serif;max-width:36rem;margin:4rem auto;padding:0 1rem;color:#0f172a;background:#eaf1ff}a{color:#1d4ed8}</style>
<h1>BrowserPier docs</h1>
<p>This BrowserPier was built without its docs. Read them at <a href="{{.}}">{{.}}</a>.</p>
</html>
`))

// placeholder answers every docs request of a binary built without them.
func (d *Docs) placeholder(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Security-Policy", cspBase+"; script-src 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	status := http.StatusOK
	if strings.Trim(r.URL.Path, "/") != "" {
		status = http.StatusNotFound
	}
	w.WriteHeader(status)
	_ = placeholderPage.Execute(w, Online)
}
