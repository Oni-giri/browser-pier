package recording

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Defaults and bounds of the admin's settings.
const (
	DefaultRetentionDays = 7
	MinRetentionDays     = 1
	MaxRetentionDays     = 90
	DefaultCapMiB        = 2048
	MinCapMiB            = 64
	MaxCapMiB            = 1 << 20 // 1 TiB
	// KeepFree is what recordings always leave free on the data dir's disk.
	KeepFree = 1 << 30
)

// Limits are what all recordings together may hold.
type Limits struct {
	Retention time.Duration // segments older than this go
	Cap       int64         // bytes, all profiles together
	KeepFree  int64         // bytes left free on the disk
}

// DefaultLimits are the limits before the admin changes any.
var DefaultLimits = Limits{Retention: DefaultRetentionDays * 24 * time.Hour, Cap: DefaultCapMiB << 20, KeepFree: KeepFree}

// Segment is one file of a recording: an MP4 that starts with a keyframe
// and plays alone. Times are the orchestrator's wall clock.
type Segment struct {
	File  string    `json:"file"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	Bytes int64     `json:"bytes"`
}

// Length is how long the segment lasts.
func (s Segment) Length() time.Duration { return s.End.Sub(s.Start) }

// Gap is a stretch of a session that wasn't recorded, and why.
type Gap struct {
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Reason Reason    `json:"reason"`
}

// Index is a session's recording: its segments and gaps, oldest first.
// It holds times, sizes and reasons only: no page text, title or URL.
type Index struct {
	Profile  string    `json:"profile"`
	Session  string    `json:"session"`
	Segments []Segment `json:"segments"`
	Gaps     []Gap     `json:"gaps,omitempty"`
}

// Start is when the first segment began.
func (ix Index) Start() time.Time {
	if len(ix.Segments) == 0 {
		return time.Time{}
	}
	return ix.Segments[0].Start
}

// End is when the last segment ended.
func (ix Index) End() time.Time {
	if len(ix.Segments) == 0 {
		return time.Time{}
	}
	return ix.Segments[len(ix.Segments)-1].End
}

// Recorded is the time the segments cover.
func (ix Index) Recorded() time.Duration {
	var d time.Duration
	for _, s := range ix.Segments {
		d += s.Length()
	}
	return d
}

// Bytes is the segments' size.
func (ix Index) Bytes() int64 {
	var n int64
	for _, s := range ix.Segments {
		n += s.Bytes
	}
	return n
}

var (
	profileRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	sessionRE = regexp.MustCompile(`^[0-9a-f]{24}$`)
	segmentRE = regexp.MustCompile(`^[0-9]{6}\.mp4$`)
	// ErrNotFound: no such recording or segment.
	ErrNotFound = errors.New("recording not found")
)

const (
	indexFile     = "index.json"
	tempSuffix    = ".tmp"
	deletedPrefix = ".deleted-"
)

// checkIDs checks a profile name and a session id; checkProfile the name alone.
func checkIDs(profile, session string) error {
	if !profileRE.MatchString(profile) || !sessionRE.MatchString(session) {
		return ErrNotFound
	}
	return nil
}

func checkProfile(profile string) error {
	if !profileRE.MatchString(profile) {
		return ErrNotFound
	}
	return nil
}

// Store is the recordings folder, <data dir>/recordings. Every path in it
// is opened through an os.Root, from ids checked against the forms
// BrowserPier gives them. The folder is made with the first segment, so
// with recording off everywhere it never exists.
type Store struct {
	dir string
	log *slog.Logger
	// Limits returns the admin's current limits.
	Limits func() Limits
	// Free returns the bytes free on the data dir's disk.
	Free func() (int64, error)
	now  func() time.Time

	mu      sync.Mutex
	root    *os.Root
	loaded  bool
	cat     map[string]map[string]*Index // by profile, then session
	pending map[string]int64             // bytes of segments being written, by profile
	writing map[string]int               // sessions with a segment being written
	seq     map[string]int               // the last segment number used, by session
}

// NewStore returns the store for dir. Nothing is created until a segment is written.
func NewStore(dir string, log *slog.Logger) *Store {
	s := &Store{dir: dir, log: log, now: time.Now,
		Limits: func() Limits { return DefaultLimits },
		cat:    map[string]map[string]*Index{}, pending: map[string]int64{}, writing: map[string]int{}, seq: map[string]int{}}
	s.Free = func() (int64, error) { return diskFree(dir) }
	return s
}

// diskFree is what the disk holding dir (or its nearest parent that
// exists) has free.
func diskFree(dir string) (int64, error) {
	for {
		var st syscall.Statfs_t
		err := syscall.Statfs(dir, &st)
		if err == nil {
			return int64(st.Bavail) * int64(st.Bsize), nil //nolint:gosec,unconvert // block counts fit in int64
		}
		parent := path.Dir(dir)
		if !errors.Is(err, syscall.ENOENT) || parent == dir {
			return 0, err
		}
		dir = parent
	}
}

// openRootLocked opens the folder, creating it if create is set. It
// returns nil, nil when the folder doesn't exist and create isn't set.
func (s *Store) openRootLocked(create bool) (*os.Root, error) {
	if s.root != nil {
		return s.root, nil
	}
	if create {
		if err := os.MkdirAll(s.dir, 0o700); err != nil {
			return nil, err
		}
	}
	r, err := os.OpenRoot(s.dir)
	if errors.Is(err, fs.ErrNotExist) && !create {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	s.root = r
	return r, nil
}

// Close lets go of the folder.
func (s *Store) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		_ = s.root.Close()
		s.root = nil
	}
}

// Reconcile loads the recordings at start: it removes those of profiles
// not in keep, folders a deletion left behind, half-written segments,
// segments the index doesn't list and index entries whose file is gone,
// then applies the retention. With no recordings folder it does nothing.
func (s *Store) Reconcile(keep []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.openRootLocked(false)
	if err != nil || root == nil {
		s.loaded = err == nil
		return err
	}
	profiles, err := readDirNames(root, ".")
	if err != nil {
		return err
	}
	s.cat = map[string]map[string]*Index{}
	for _, p := range profiles {
		if !slices.Contains(keep, p) || !profileRE.MatchString(p) {
			if err := root.RemoveAll(p); err != nil {
				s.log.Warn("removing the recordings of a profile that's gone", "folder", sanitize(p), "err", err)
			}
			continue
		}
		sessions, err := readDirNames(root, p)
		if err != nil {
			return err
		}
		for _, id := range sessions {
			dir := p + "/" + id
			if !sessionRE.MatchString(id) {
				_ = root.RemoveAll(dir)
				continue
			}
			ix := s.loadIndexLocked(root, p, id)
			if ix == nil || len(ix.Segments) == 0 {
				_ = root.RemoveAll(dir)
				continue
			}
			s.putLocked(ix)
		}
		if len(s.cat[p]) == 0 {
			_ = root.Remove(p)
		}
	}
	s.loaded = true
	s.sweepLocked()
	return nil
}

// loadIndexLocked reads a session's index and makes the folder match it:
// files it doesn't list go, entries without a file go.
func (s *Store) loadIndexLocked(root *os.Root, profile, id string) *Index {
	dir := profile + "/" + id
	b, err := root.ReadFile(dir + "/" + indexFile)
	var ix Index
	if err != nil || json.Unmarshal(b, &ix) != nil {
		return nil
	}
	ix.Profile, ix.Session = profile, id
	listed := map[string]bool{}
	kept := ix.Segments[:0]
	for _, seg := range ix.Segments {
		if !segmentRE.MatchString(seg.File) {
			continue
		}
		fi, err := root.Stat(dir + "/" + seg.File)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		seg.Bytes = fi.Size()
		listed[seg.File] = true
		kept = append(kept, seg)
		if n, _ := strconv.Atoi(strings.TrimSuffix(seg.File, ".mp4")); n > s.seq[id] {
			s.seq[id] = n
		}
	}
	ix.Segments = kept
	names, _ := readDirNames(root, dir)
	for _, n := range names {
		if n != indexFile && !listed[n] {
			_ = root.RemoveAll(dir + "/" + n)
		}
	}
	return &ix
}

func readDirNames(root *os.Root, dir string) ([]string, error) {
	f, err := root.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	names, err := f.Readdirnames(-1)
	sort.Strings(names)
	return names, err
}

func (s *Store) putLocked(ix *Index) {
	if s.cat[ix.Profile] == nil {
		s.cat[ix.Profile] = map[string]*Index{}
	}
	s.cat[ix.Profile][ix.Session] = ix
}

func (s *Store) usageLocked() (total int64, by map[string]int64) {
	by = map[string]int64{}
	for p, sessions := range s.cat {
		for _, ix := range sessions {
			by[p] += ix.Bytes()
		}
	}
	for p, n := range s.pending {
		by[p] += n
	}
	for _, n := range by {
		total += n
	}
	return total, by
}

// Usage is what recordings hold, in bytes, by profile and in all.
func (s *Store) Usage() (total int64, byProfile map[string]int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.usageLocked()
}

// reserve makes room for n more bytes of profile's segment being written:
// within the cap, and leaving KeepFree on the disk. When either is
// reached, the profile holding the most gives up its oldest segment, again
// until there is room; one with only a segment being written gives up
// nothing, so a busy profile can't wipe a quiet one's recordings. It
// reports false when there is no room.
func (s *Store) reserve(profile string, n int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	lim := s.Limits()
	for {
		total, by := s.usageLocked()
		free, err := s.Free()
		if err != nil {
			free = 1 << 62 // can't tell: only the cap applies
		}
		if total+n <= lim.Cap && free-n >= lim.KeepFree {
			s.pending[profile] += n
			return true
		}
		// The profile holding the most, by name on a tie.
		var most string
		for p, b := range by {
			if most == "" || b > by[most] || (b == by[most] && p < most) {
				most = p
			}
		}
		if most == "" || !s.dropOldestLocked(most, Lost) {
			return false
		}
	}
}

// dropOldestLocked removes profile's oldest segment, leaving a gap with
// why in its place. It reports false if the profile has none.
func (s *Store) dropOldestLocked(profile string, why Reason) bool {
	var oldest *Index
	for _, ix := range s.cat[profile] {
		if len(ix.Segments) > 0 && (oldest == nil || ix.Segments[0].Start.Before(oldest.Segments[0].Start)) {
			oldest = ix
		}
	}
	if oldest == nil || s.root == nil {
		return false
	}
	seg := oldest.Segments[0]
	if err := s.root.Remove(profile + "/" + oldest.Session + "/" + seg.File); err != nil && !errors.Is(err, fs.ErrNotExist) {
		s.log.Warn("removing a recording segment", "profile", profile, "session", oldest.Session, "err", err)
		return false
	}
	oldest.Segments = oldest.Segments[1:]
	if why != "" && len(oldest.Segments) > 0 {
		oldest.Gaps = append(oldest.Gaps, Gap{Start: seg.Start, End: seg.End, Reason: why})
	}
	s.saveOrRemoveLocked(oldest)
	return true
}

// saveOrRemoveLocked writes a session's index, or removes its folder
// once it has no segment left and none is being written.
func (s *Store) saveOrRemoveLocked(ix *Index) {
	dir := ix.Profile + "/" + ix.Session
	if len(ix.Segments) == 0 && s.writing[ix.Session] == 0 {
		delete(s.cat[ix.Profile], ix.Session)
		if err := s.root.RemoveAll(dir); err != nil {
			s.log.Warn("removing a recording", "profile", ix.Profile, "session", ix.Session, "err", err)
		}
		if len(s.cat[ix.Profile]) == 0 {
			delete(s.cat, ix.Profile)
			_ = s.root.Remove(ix.Profile)
		}
		return
	}
	if err := s.writeIndexLocked(ix); err != nil {
		s.log.Warn("saving a recording's index", "profile", ix.Profile, "session", ix.Session, "err", err)
	}
}

func (s *Store) writeIndexLocked(ix *Index) error {
	sort.Slice(ix.Gaps, func(i, j int) bool { return ix.Gaps[i].Start.Before(ix.Gaps[j].Start) })
	b, err := json.Marshal(ix)
	if err != nil {
		return err
	}
	dir := ix.Profile + "/" + ix.Session
	tmp := dir + "/" + indexFile + tempSuffix
	if err := s.root.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return s.root.Rename(tmp, dir+"/"+indexFile)
}

// draft is a segment being written, under a temporary name until commit.
type draft struct {
	s                *Store
	profile, session string
	name             string // the final name, 000001.mp4
	f                *os.File
	bytes            int64 // written so far, all reserved
}

// create starts a session's next segment, reserving its first n bytes.
// It returns errNoRoom if the limits leave none.
func (s *Store) create(profile, session string, first []byte) (*draft, error) {
	if err := checkIDs(profile, session); err != nil {
		return nil, err
	}
	if !s.reserve(profile, int64(len(first))) {
		return nil, errNoRoom
	}
	d, err := s.createReserved(profile, session)
	if err != nil {
		s.unreserve(profile, int64(len(first)))
		return nil, err
	}
	if err := d.writeReserved(first); err != nil {
		d.abort()
		return nil, err
	}
	return d, nil
}

var errNoRoom = errors.New("no room for recordings within their limits")

func (s *Store) createReserved(profile, session string) (*draft, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.openRootLocked(true)
	if err != nil {
		return nil, err
	}
	if !s.loaded {
		// A session's numbering goes on from what is there.
		if ix := s.loadIndexLocked(root, profile, session); ix != nil && len(ix.Segments) > 0 {
			s.putLocked(ix)
		}
	}
	dir := profile + "/" + session
	if err := root.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s.seq[session]++
	name := fmt.Sprintf("%06d.mp4", s.seq[session])
	f, err := root.OpenFile(dir+"/"+name+tempSuffix, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	s.writing[session]++
	return &draft{s: s, profile: profile, session: session, name: name, f: f}, nil
}

func (s *Store) unreserve(profile string, n int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.unreserveLocked(profile, n)
}

func (s *Store) unreserveLocked(profile string, n int64) {
	if s.pending[profile] -= n; s.pending[profile] <= 0 {
		delete(s.pending, profile)
	}
}

// write adds b to the segment, if there is room for it.
func (d *draft) write(b []byte) error {
	if !d.s.reserve(d.profile, int64(len(b))) {
		return errNoRoom
	}
	return d.writeReserved(b)
}

func (d *draft) writeReserved(b []byte) error {
	n, err := d.f.Write(b)
	d.bytes += int64(len(b))
	if err == nil && n < len(b) {
		err = errors.New("short write")
	}
	return err
}

// abort removes the segment.
func (d *draft) abort() {
	_ = d.f.Close()
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		_ = s.root.Remove(d.profile + "/" + d.session + "/" + d.name + tempSuffix)
	}
	s.unreserveLocked(d.profile, d.bytes)
	s.doneWritingLocked(d.profile, d.session)
}

func (s *Store) doneWritingLocked(profile, session string) {
	if s.writing[session]--; s.writing[session] <= 0 {
		delete(s.writing, session)
		if ix := s.cat[profile][session]; ix == nil && s.root != nil {
			// Nothing was ever kept for this session: no folder either.
			if names, _ := readDirNames(s.root, profile+"/"+session); len(names) == 0 {
				_ = s.root.Remove(profile + "/" + session)
				if names, _ := readDirNames(s.root, profile); len(names) == 0 {
					_ = s.root.Remove(profile)
				}
			}
		}
	}
}

// commit makes the segment whole under its final name and adds it to the
// session's index with the gaps before it.
func (d *draft) commit(start, end time.Time, gaps []Gap) error {
	syncErr := d.f.Sync()
	closeErr := d.f.Close()
	s := d.s
	s.mu.Lock()
	defer s.mu.Unlock()
	defer s.doneWritingLocked(d.profile, d.session)
	s.unreserveLocked(d.profile, d.bytes)
	dir := d.profile + "/" + d.session
	if err := errors.Join(syncErr, closeErr); err != nil {
		_ = s.root.Remove(dir + "/" + d.name + tempSuffix)
		return err
	}
	if err := s.root.Rename(dir+"/"+d.name+tempSuffix, dir+"/"+d.name); err != nil {
		_ = s.root.Remove(dir + "/" + d.name + tempSuffix)
		return err
	}
	ix := s.cat[d.profile][d.session]
	if ix == nil {
		ix = &Index{Profile: d.profile, Session: d.session}
		s.putLocked(ix)
	}
	ix.Segments = append(ix.Segments, Segment{File: d.name, Start: start, End: end, Bytes: d.bytes})
	ix.Gaps = append(ix.Gaps, gaps...)
	return s.writeIndexLocked(ix)
}

// addGaps records gaps after a session's last segment, as it ends.
func (s *Store) addGaps(profile, session string, gaps []Gap) {
	if len(gaps) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ix := s.cat[profile][session]; ix != nil && s.root != nil {
		ix.Gaps = append(ix.Gaps, gaps...)
		if err := s.writeIndexLocked(ix); err != nil {
			s.log.Warn("saving a recording's index", "profile", profile, "session", session, "err", err)
		}
	}
}

// Sweep removes segments older than the retention, and sessions and
// profiles left with none.
func (s *Store) Sweep() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.root != nil {
		s.sweepLocked()
	}
}

// A segment past the cutoff means the profile's oldest one is too, so
// dropping the profile's oldest until this session's first is newer
// removes only segments past the cutoff.
func (s *Store) sweepLocked() {
	cutoff := s.now().Add(-s.Limits().Retention)
	for _, sessions := range s.cat {
		for _, ix := range sessions {
			for len(ix.Segments) > 0 && ix.Segments[0].End.Before(cutoff) {
				if !s.dropOldestLocked(ix.Profile, "") {
					break
				}
				if s.cat[ix.Profile][ix.Session] != ix {
					break // the session went with its last segment
				}
			}
			if len(ix.Gaps) > 0 && s.cat[ix.Profile][ix.Session] == ix {
				kept := ix.Gaps[:0]
				for _, g := range ix.Gaps {
					if !g.End.Before(cutoff) {
						kept = append(kept, g)
					}
				}
				if len(kept) != len(ix.Gaps) {
					ix.Gaps = kept
					s.saveOrRemoveLocked(ix)
				}
			}
		}
	}
}

// Sessions lists a profile's recordings, newest first.
func (s *Store) Sessions(profile string) []Index {
	if checkProfile(profile) != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Index
	for _, ix := range s.cat[profile] {
		if len(ix.Segments) > 0 {
			out = append(out, copyIndex(ix))
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start().After(out[j].Start()) })
	return out
}

func copyIndex(ix *Index) Index {
	c := *ix
	c.Segments = slices.Clone(ix.Segments)
	c.Gaps = slices.Clone(ix.Gaps)
	return c
}

// Get returns one session's recording.
func (s *Store) Get(profile, session string) (Index, error) {
	if err := checkIDs(profile, session); err != nil {
		return Index{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ix := s.cat[profile][session]
	if ix == nil || len(ix.Segments) == 0 {
		return Index{}, ErrNotFound
	}
	return copyIndex(ix), nil
}

// Open opens one segment for reading. The file name must be one the
// index lists.
func (s *Store) Open(profile, session, file string) (*os.File, Segment, error) {
	if err := checkIDs(profile, session); err != nil || !segmentRE.MatchString(file) {
		return nil, Segment{}, ErrNotFound
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ix := s.cat[profile][session]
	if ix == nil || s.root == nil {
		return nil, Segment{}, ErrNotFound
	}
	for _, seg := range ix.Segments {
		if seg.File == file {
			f, err := s.root.Open(profile + "/" + session + "/" + file)
			if errors.Is(err, fs.ErrNotExist) {
				return nil, Segment{}, ErrNotFound
			}
			return f, seg, err
		}
	}
	return nil, Segment{}, ErrNotFound
}

// Delete removes one session's recording. A segment being written is
// removed when it ends.
func (s *Store) Delete(profile, session string) error {
	if err := checkIDs(profile, session); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	ix := s.cat[profile][session]
	if ix == nil || s.root == nil {
		return ErrNotFound
	}
	for len(ix.Segments) > 0 {
		if !s.dropOldestInLocked(ix) {
			return fmt.Errorf("removing a segment of recording %s", session)
		}
	}
	return nil
}

func (s *Store) dropOldestInLocked(ix *Index) bool {
	seg := ix.Segments[0]
	if err := s.root.Remove(ix.Profile + "/" + ix.Session + "/" + seg.File); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return false
	}
	ix.Segments = ix.Segments[1:]
	s.saveOrRemoveLocked(ix)
	return true
}

// DeleteAll removes every recording of a profile.
func (s *Store) DeleteAll(profile string) error {
	if err := checkProfile(profile); err != nil {
		return err
	}
	s.mu.Lock()
	ids := make([]string, 0, len(s.cat[profile]))
	for id := range s.cat[profile] {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	var errs []error
	for _, id := range ids {
		if err := s.Delete(profile, id); err != nil && !errors.Is(err, ErrNotFound) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// MoveAside takes a profile's recordings out of the store, for a profile
// deletion: restore puts them back if the deletion fails, remove deletes
// them for good once it has succeeded. A profile with none returns
// no-ops. Reconcile removes what a crash leaves aside.
func (s *Store) MoveAside(profile, tag string) (restore func() error, remove func(), err error) {
	noop := func() error { return nil }
	if err := checkProfile(profile); err != nil {
		return nil, nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	root, err := s.openRootLocked(false)
	if err != nil {
		return nil, nil, err
	}
	if root == nil {
		return noop, func() {}, nil
	}
	if _, err := root.Stat(profile); errors.Is(err, fs.ErrNotExist) {
		return noop, func() {}, nil
	}
	if len(s.pending) > 0 && s.pending[profile] > 0 {
		return nil, nil, errors.New("a recording of this profile is still being written")
	}
	aside := deletedPrefix + profile + "-" + tag
	if err := root.Rename(profile, aside); err != nil {
		return nil, nil, fmt.Errorf("moving the profile's recordings aside: %w", err)
	}
	held := s.cat[profile]
	delete(s.cat, profile)
	restore = func() error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := root.Rename(aside, profile); err != nil {
			return err
		}
		if held != nil {
			s.cat[profile] = held
		}
		return nil
	}
	remove = func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if err := root.RemoveAll(aside); err != nil {
			s.log.Warn("removing a deleted profile's recordings; will retry at the next start", "profile", profile, "err", err)
		}
	}
	return restore, remove, nil
}
