package blocklist

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

// DefaultURLs are the lists used when BROWSERPIER_BLOCKLIST_URLS isn't
// set: conservative ones, made of ad and tracker servers only, so few
// pages break. Peter Lowe's list is in the hosts format; EasyPrivacy is an
// Adblock Plus list, of which only the plain ||name^ rules (its tracking
// servers) are kept.
var DefaultURLs = []string{
	"https://pgl.yoyo.org/adservers/serverlist.php?hostformat=hosts&showintro=0&mimetype=plaintext",
	"https://easylist.to/easylist/easyprivacy.txt",
}

const (
	// RefreshEvery is how old a list may get before it is downloaded again.
	RefreshEvery = 4 * 24 * time.Hour
	// retryEvery is how soon a list that failed to download is tried again.
	retryEvery = time.Hour
	// fetchTimeout bounds one download.
	fetchTimeout = 2 * time.Minute
)

// Lists are the block lists, as last downloaded, and the set compiled from
// them. Its methods are safe for concurrent use.
type Lists struct {
	dir    string // where the lists and their metadata are kept
	urls   []string
	client *http.Client
	log    *slog.Logger
	now    func() time.Time

	mu       sync.RWMutex
	set      *domainset.Set
	file     []byte // set's file, as containers get it
	stats    Stats
	checked  map[string]time.Time // when each list was last tried, by URL
	refresh  sync.Mutex           // one refresh at a time
	reported bool                 // the missing-list warning was logged
}

// meta is what is kept beside each list, for conditional requests.
type meta struct {
	URL          string    `json:"url"`
	ETag         string    `json:"etag,omitempty"`
	LastModified string    `json:"last_modified,omitempty"`
	Fetched      time.Time `json:"fetched"` // last 200 or 304
}

// New returns the lists at urls, kept in dir. Call Load, then Run.
func New(dir string, urls []string, log *slog.Logger) (*Lists, error) {
	for _, u := range urls {
		p, err := url.Parse(u)
		if err != nil || (p.Scheme != "http" && p.Scheme != "https") || p.Host == "" {
			return nil, fmt.Errorf("block list %q: want an http or https URL", u)
		}
	}
	return &Lists{
		dir: dir, urls: urls, log: log, now: time.Now,
		client:  &http.Client{Timeout: fetchTimeout},
		checked: map[string]time.Time{},
	}, nil
}

// ParseURLs reads BROWSERPIER_BLOCKLIST_URLS: URLs separated by commas or
// spaces; unset means DefaultURLs, and "none" (or only separators) no list.
func ParseURLs(v string, set bool) []string {
	if !set {
		return DefaultURLs
	}
	if strings.TrimSpace(v) == "none" {
		return nil
	}
	return strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
}

// base is the file name a list is kept under: a hash of its URL, so any
// URL makes a safe name.
func base(u string) string {
	h := sha256.Sum256([]byte(u))
	return hex.EncodeToString(h[:8])
}

// Load compiles the lists already on disk, without the network, so
// browsers block from the first start after a restart.
func (l *Lists) Load() error {
	if err := os.MkdirAll(l.dir, 0o700); err != nil {
		return err
	}
	return l.compile()
}

// Set is the compiled set; nil before any list was read.
func (l *Lists) Set() *domainset.Set {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.set
}

// File is the compiled set as a domainset file, for a browser container;
// nil when no list was ever downloaded. A browser then starts without
// one, and the first time that happens is logged.
func (l *Lists) File() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.set.Len() == 0 && len(l.urls) > 0 && !l.reported {
		l.reported = true
		l.log.Warn("no ad and tracker list has been downloaded yet: browsers start without blocking, and the lists are tried again every hour")
	}
	if l.set.Len() == 0 {
		return nil
	}
	return l.file
}

// Stats is what compiling the lists on disk kept and skipped.
func (l *Lists) Stats() Stats {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.stats
}

// Run refreshes the lists when they are due, until ctx ends: at once for
// a list never downloaded, then every RefreshEvery, or retryEvery after a
// failed download. It checks every few minutes.
func (l *Lists) Run(ctx context.Context, every time.Duration) {
	for {
		l.Refresh(ctx, false)
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}

// Refresh downloads each list that is due (all of them with force), with
// a conditional request when it has one. A 304, or any failure, keeps the
// list as it was. The set is compiled again if any list changed.
func (l *Lists) Refresh(ctx context.Context, force bool) {
	l.refresh.Lock()
	defer l.refresh.Unlock()
	changed := false
	for _, u := range l.urls {
		m, _ := l.readMeta(u)
		now := l.now()
		l.mu.RLock()
		tried := l.checked[u]
		l.mu.RUnlock()
		due := m.Fetched.IsZero() || now.Sub(m.Fetched) >= RefreshEvery
		if !force && (!due || (!tried.IsZero() && now.Sub(tried) < retryEvery)) {
			continue
		}
		l.mu.Lock()
		l.checked[u] = now
		l.mu.Unlock()
		got, err := l.fetch(ctx, u, m)
		if err != nil {
			if ctx.Err() == nil {
				l.log.Warn("downloading an ad and tracker list failed; keeping the last one", "list", u, "err", err)
			}
			continue
		}
		changed = changed || got
	}
	if changed {
		if err := l.compile(); err != nil {
			l.log.Warn("compiling the ad and tracker lists failed; keeping the last set", "err", err)
		}
	}
}

// fetch downloads u, conditionally on m. It reports whether the list
// changed; on a 304 only its metadata does.
func (l *Lists) fetch(ctx context.Context, u string, m meta) (bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return false, err
	}
	_, statErr := os.Stat(filepath.Join(l.dir, base(u)+".txt"))
	if statErr == nil {
		if m.ETag != "" {
			req.Header.Set("If-None-Match", m.ETag)
		}
		if m.LastModified != "" {
			req.Header.Set("If-Modified-Since", m.LastModified)
		}
	}
	req.Header.Set("User-Agent", "BrowserPier")
	resp, err := l.client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch {
	case resp.StatusCode == http.StatusNotModified && statErr == nil:
		m.URL, m.Fetched = u, l.now()
		return false, l.writeMeta(u, m)
	case resp.StatusCode != http.StatusOK:
		return false, fmt.Errorf("the server answered %s", resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxListBytes+1))
	if err != nil {
		return false, err
	}
	if len(body) > maxListBytes {
		return false, fmt.Errorf("the list is over %d MB", maxListBytes>>20)
	}
	// A list in which nothing parses is a broken download (an error page
	// served as 200, say), not an empty list.
	st, err := Parse(bytes.NewReader(body), func(string) {})
	if err != nil {
		return false, err
	}
	if st.Names == 0 {
		return false, fmt.Errorf("no name in it (%d lines)", st.Lines)
	}
	if err := writeFile(filepath.Join(l.dir, base(u)+".txt"), body); err != nil {
		return false, err
	}
	m = meta{URL: u, ETag: resp.Header.Get("ETag"), LastModified: resp.Header.Get("Last-Modified"), Fetched: l.now()}
	return true, l.writeMeta(u, m)
}

// compile builds the set from the lists on disk, in the order of urls.
// A list missing on disk is left out.
func (l *Lists) compile() error {
	c := newCompiler()
	var total Stats
	for _, u := range l.urls {
		f, err := os.Open(filepath.Join(l.dir, base(u)+".txt")) //nolint:gosec // our own file name
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		st, err := Parse(io.LimitReader(f, maxListBytes), c.add)
		_ = f.Close()
		if err != nil {
			return err
		}
		total.add(st)
	}
	set := c.set()
	var buf bytes.Buffer
	if _, err := set.WriteTo(&buf); err != nil {
		return err
	}
	if c.over > 0 {
		l.log.Warn("the ad and tracker lists hold more names than a browser takes; the rest are left out", "kept", set.Len(), "left_out", c.over)
	}
	l.mu.Lock()
	l.set, l.file, l.stats = set, buf.Bytes(), total
	l.mu.Unlock()
	if set.Len() > 0 {
		l.log.Info("ad and tracker lists compiled", "names", set.Len(), "lines", total.Lines,
			"skipped", total.Skipped, "invalid", total.Invalid, "too_long", total.Long)
	}
	return nil
}

func (l *Lists) readMeta(u string) (meta, error) {
	var m meta
	b, err := os.ReadFile(filepath.Join(l.dir, base(u)+".json")) //nolint:gosec // our own file name
	if err != nil {
		return m, err
	}
	if err := json.Unmarshal(b, &m); err != nil || m.URL != u {
		return meta{}, errors.New("unreadable list metadata")
	}
	return m, nil
}

func (l *Lists) writeMeta(u string, m meta) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return writeFile(filepath.Join(l.dir, base(u)+".json"), b)
}

// writeFile replaces path with b at once, so a crash leaves the old file
// or the new one, never half of one.
func writeFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		_ = os.Remove(f.Name())
	}
	return err
}
