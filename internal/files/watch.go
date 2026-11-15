package files

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Oni-giri/browser-pier/internal/fence"
)

// Download notices (PROJ-116): an action that may start a download (act,
// a click, a key press) lists the downloads folder before it runs, and
// again after, and says what appeared. No CDP is involved, so it works the
// same in both modes: Chrome writes a download as "Unconfirmed
// N.crdownload" (or name.crdownload) and renames it to its name once it
// is finished, so a new .crdownload is a download under way, and a new
// file without the suffix one that finished.
//
// Chrome creates the file only once the server has answered, which can be
// after the action has returned: a notice looks for StartGrace after the
// action for a download to start, and whatever finished between one
// action of a session and the next is told with the next (Earlier).

const (
	// DefaultDownloadWait is how long an action waits for a download it
	// started to finish, when it doesn't say; MaxDownloadWait caps what it
	// may ask for.
	DefaultDownloadWait = 3 * time.Second
	MaxDownloadWait     = 30 * time.Second
	// MaxNotices caps the files a notice names; the rest are counted.
	MaxNotices = 5
	// StartGrace is how long after an action its notice keeps looking
	// for a download to start, so that an action that downloads nothing
	// is held up by no more than this; a download that starts later is
	// told with the session's next action.
	StartGrace = 40 * time.Millisecond
	// downloadPoll is how often a download under way is looked at, and
	// gracePoll how often one is looked for during StartGrace.
	downloadPoll = 200 * time.Millisecond
	gracePoll    = 10 * time.Millisecond
)

// unconfirmed is how Chrome names a download until it is finished, when
// it gets its name.
var unconfirmed = regexp.MustCompile(`^Unconfirmed [0-9]+$`)

// DownloadState is what became of a file that appeared during an action.
type DownloadState string

const (
	// Downloaded: a finished file, there at the end.
	Downloaded DownloadState = "downloaded"
	// InProgress: still being written when the wait ran out.
	InProgress DownloadState = "in_progress"
	// Vanished: a download under way that went without finishing
	// (cancelled, failed, or removed).
	Vanished DownloadState = "vanished"
	// Removed: a file that finished, then went before the end, as the
	// files cap's eviction can do.
	Removed DownloadState = "removed"
)

// Download is one file an action's notice names. Name comes from the page
// that offered the download: it is cleaned of invisible characters, but
// still page text, to be fenced for an agent and escaped for a person.
type Download struct {
	// Name is "" for a download in progress whose name Chrome hasn't
	// given it yet.
	Name  string        `json:"name"`
	Size  int64         `json:"size"` // bytes: so far, for one in progress
	State DownloadState `json:"state"`
	// Earlier marks a file that finished after the session's last action
	// had answered, before this one: a download that started late, or
	// one the last action said was in progress.
	Earlier bool `json:"earlier,omitempty"`
}

// seen is a regular file in the downloads folder, with what os.SameFile
// needs to follow it through Chrome's renames.
type seen struct {
	File
	info fs.FileInfo
}

func (e seen) partial() bool { return strings.HasSuffix(e.Name, partial) }

// Mark is the downloads folder as it was before an action.
type Mark struct {
	s                *Store
	profile, session string
	before           map[string]seen // by name; nil if the folder couldn't be read
	earlier          []Download      // finished since the session's last notice
	ended            time.Time       // when the action ended (Ended)
	poll, gracePoll  time.Duration
	grace            time.Duration
}

// Mark lists the profile's downloads folder before an action of the
// session. It never fails: a folder that can't be read is reported as
// nothing new, since an action must not fail over its notice.
func (s *Store) Mark(profile, session string) *Mark {
	m := &Mark{s: s, profile: profile, session: session, poll: downloadPoll, gracePoll: gracePoll, grace: StartGrace}
	list, err := s.downloads(profile)
	if err != nil {
		return m
	}
	m.before = list
	s.mu.Lock()
	prev := s.seen[session]
	s.mu.Unlock()
	if prev == nil {
		return m // the session's first: nothing to tell from before it
	}
	var at []time.Time
	for name, e := range list {
		if !e.partial() && fresh(prev, e) {
			m.earlier = append(m.earlier, Download{Name: cleanName(name), Size: e.Size, State: Downloaded, Earlier: true})
			at = append(at, e.Modified)
		}
	}
	sort.Sort(byTime{m.earlier, at})
	return m
}

// Ended records that the action has ended: StartGrace counts from here.
// Without it, it counts from Downloads.
func (m *Mark) Ended() {
	if m != nil {
		m.ended = time.Now()
	}
}

// Forget drops what the store keeps about a session's downloads folder,
// once the session has ended.
func (s *Store) Forget(session string) {
	s.mu.Lock()
	delete(s.seen, session)
	s.mu.Unlock()
}

// remember keeps the listing a session's notice ended with, for the next
// action's Earlier.
func (s *Store) remember(session string, list map[string]seen) {
	if session == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.seen == nil {
		s.seen = map[string]map[string]seen{}
	}
	s.seen[session] = list
}

// downloads lists every regular file in the downloads folder, partial
// ones too, through os.Root. A missing folder is empty.
func (s *Store) downloads(profile string) (map[string]seen, error) {
	root, err := s.open(profile, Downloads)
	if errors.Is(err, ErrNotFound) {
		return map[string]seen{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = root.Close() }()
	d, err := root.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	entries, err := d.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	out := make(map[string]seen, len(entries))
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue // a symlink, a folder, a FIFO: never a download
		}
		info, err := e.Info() // lstat: never follows a link
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		out[e.Name()] = seen{File{Name: e.Name(), Size: info.Size(), Modified: info.ModTime()}, info}
	}
	return out, nil
}

// fresh reports whether e wasn't in the before listing: a new name, or
// the same name for another file or a rewritten one.
func fresh(before map[string]seen, e seen) bool {
	b, ok := before[e.Name]
	if !ok {
		// A partial download that was there before, renamed, is still
		// the same download.
		if e.partial() {
			for _, b := range before {
				if b.partial() && os.SameFile(b.info, e.info) {
					return false
				}
			}
		}
		return true
	}
	if e.partial() {
		return !os.SameFile(b.info, e.info)
	}
	return !os.SameFile(b.info, e.info) || b.Size != e.Size || !b.Modified.Equal(e.Modified)
}

// Downloads lists the folder again after the action and reports what
// appeared: first what finished since the session's last notice, then
// finished files, and downloads under way. If nothing has appeared it
// keeps looking until StartGrace after the action ended; only when a new
// partial download is there does it wait longer, looking every 200 ms for
// up to wait for it to finish. A download that was already under way
// before the action isn't waited for or reported, unless it finished
// during it. It never fails: a folder it can't read, or ctx ending,
// reports what it has.
func (m *Mark) Downloads(ctx context.Context, wait time.Duration) []Download {
	if m == nil || m.before == nil {
		return nil
	}
	now := time.Now()
	deadline := now.Add(wait)
	graceEnd := m.ended
	if graceEnd.IsZero() {
		graceEnd = now
	}
	graceEnd = graceEnd.Add(m.grace)
	finished := map[string]seen{} // new finished files, as last seen
	started := map[string]seen{}  // new partial downloads ever seen
	var last map[string]seen
	for {
		cur, err := m.s.downloads(m.profile)
		if err != nil {
			break // keep the last listing
		}
		last = cur
		active := 0
		for name, e := range cur {
			if !fresh(m.before, e) {
				continue
			}
			if e.partial() {
				started[name] = e
				active++
			} else {
				finished[name] = e
			}
		}
		poll := m.poll
		switch {
		case active > 0 && time.Now().Add(m.poll).Before(deadline):
		case len(finished) == 0 && len(started) == 0 && time.Now().Before(graceEnd):
			poll = m.gracePoll
		default:
			poll = 0
		}
		if poll == 0 {
			break
		}
		t := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			t.Stop()
		case <-t.C:
		}
		if ctx.Err() != nil {
			break
		}
	}
	if last == nil {
		return m.earlier
	}
	m.s.remember(m.session, last)
	return append(m.earlier, report(last, finished, started)...)
}

// report sorts what was seen into a notice: finished files (oldest
// first), then downloads still under way, then those that vanished.
func report(last, finished, started map[string]seen) []Download {
	var done, running, gone []Download
	var doneAt []time.Time
	for name, e := range finished {
		if now, ok := last[name]; ok && os.SameFile(now.info, e.info) {
			done = append(done, Download{Name: cleanName(name), Size: now.Size, State: Downloaded})
			doneAt = append(doneAt, now.Modified)
		} else {
			done = append(done, Download{Name: cleanName(name), Size: e.Size, State: Removed})
			doneAt = append(doneAt, e.Modified)
		}
	}
	sort.Sort(byTime{done, doneAt})
	for name, e := range started {
		if now, ok := last[name]; ok && os.SameFile(now.info, e.info) {
			running = append(running, Download{Name: partialName(name), Size: now.Size, State: InProgress})
			continue
		}
		if becameFile(e, last, finished) {
			continue // renamed: finished, or under another partial name
		}
		gone = append(gone, Download{Name: partialName(name), Size: e.Size, State: Vanished})
	}
	sort.Slice(running, func(i, j int) bool { return running[i].Name < running[j].Name })
	sort.Slice(gone, func(i, j int) bool { return gone[i].Name < gone[j].Name })
	return append(append(done, running...), gone...)
}

// becameFile reports whether a partial download went on under another
// name: renamed to its finished name, or to another partial one.
func becameFile(p seen, last, finished map[string]seen) bool {
	if _, ok := finished[strings.TrimSuffix(p.Name, partial)]; ok {
		return true
	}
	for _, e := range finished {
		if os.SameFile(e.info, p.info) {
			return true
		}
	}
	for _, e := range last {
		if os.SameFile(e.info, p.info) {
			return true
		}
	}
	return false
}

type byTime struct {
	d  []Download
	at []time.Time
}

func (b byTime) Len() int { return len(b.d) }
func (b byTime) Less(i, j int) bool {
	if !b.at[i].Equal(b.at[j]) {
		return b.at[i].Before(b.at[j])
	}
	return b.d[i].Name < b.d[j].Name
}
func (b byTime) Swap(i, j int) {
	b.d[i], b.d[j] = b.d[j], b.d[i]
	b.at[i], b.at[j] = b.at[j], b.at[i]
}

// partialName is the name a partial download will have, or "" while
// Chrome calls it "Unconfirmed N".
func partialName(name string) string {
	name = strings.TrimSuffix(name, partial)
	if unconfirmed.MatchString(name) {
		return ""
	}
	return cleanName(name)
}

// cleanName strips what a reader wouldn't see (fence.Clean: zero-width
// and bidi characters among them) and puts any line break or tab left as
// a space, so a name is one line of a notice.
func cleanName(name string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return ' '
		}
		return r
	}, fence.Clean(name))
}

// Line is how a notice names d to an agent, e.g. "downloaded report.csv
// (12 kB)". The name is page text: the caller fences it.
func (d Download) Line() string {
	switch d.State {
	case InProgress:
		if d.Name == "" {
			return fmt.Sprintf("download in progress (%s so far; it gets its name when it finishes); list_files to check later", Size(d.Size))
		}
		return fmt.Sprintf("download in progress: %s (%s so far); list_files to check later", d.Name, Size(d.Size))
	case Vanished:
		return "a download vanished before it finished"
	case Removed:
		return fmt.Sprintf("downloaded %s (%s), then removed (the files cap evicts the oldest files)", d.Name, Size(d.Size))
	}
	if d.Earlier {
		return fmt.Sprintf("downloaded %s (%s), after the last action's answer", d.Name, Size(d.Size))
	}
	return fmt.Sprintf("downloaded %s (%s)", d.Name, Size(d.Size))
}

// Lines is a notice's lines, at most MaxNotices, then "and N more".
func Lines(ds []Download) []string {
	var out []string
	for i, d := range ds {
		if i == MaxNotices {
			out = append(out, fmt.Sprintf("and %d more", len(ds)-MaxNotices))
			break
		}
		out = append(out, d.Line())
	}
	return out
}

// Size puts a byte count in words: 512 B, 4.1 kB, 12 kB, 4.1 MB.
func Size(n int64) string {
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	for _, unit := range []string{"kB", "MB", "GB", "TB"} {
		v /= 1000
		if v < 999.5 || unit == "TB" {
			if v < 9.95 {
				return fmt.Sprintf("%.1f %s", v, unit)
			}
			return fmt.Sprintf("%.0f %s", v, unit)
		}
	}
	return fmt.Sprintf("%d B", n) // not reached
}

// DownloadWait is the wait an action asks for in download_wait_ms: unset
// is DefaultDownloadWait, 0 doesn't wait, and more than MaxDownloadWait
// is an error.
func DownloadWait(ms *int) (time.Duration, error) {
	if ms == nil {
		return DefaultDownloadWait, nil
	}
	if *ms < 0 || time.Duration(*ms)*time.Millisecond > MaxDownloadWait {
		return 0, fmt.Errorf("download_wait_ms is 0 to %d", MaxDownloadWait.Milliseconds())
	}
	return time.Duration(*ms) * time.Millisecond, nil
}

// LogLine is how the activity log names d, e.g. "Downloaded report.csv
// (12 kB)", or "" for a download that didn't finish. The name is page
// text: the web UI escapes it.
func (d Download) LogLine() string {
	if d.State != Downloaded && d.State != Removed {
		return ""
	}
	return fmt.Sprintf("Downloaded %s (%s)", d.Name, Size(d.Size))
}
