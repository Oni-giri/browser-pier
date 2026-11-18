package recording

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func box(typ string, payload []byte) []byte {
	b := make([]byte, 8, 8+len(payload))
	binary.BigEndian.PutUint32(b, uint32(8+len(payload))) //nolint:gosec // small in tests
	copy(b[4:], typ)
	return append(b, payload...)
}

var testInit = slices2(box("ftyp", []byte("isomiso2")), box("moov", bytes.Repeat([]byte{1}, 64)))

func slices2(a, b []byte) []byte { return append(append([]byte{}, a...), b...) }

// frag is a fragment whose mdat holds n bytes of mark.
func frag(mark byte, n int) []byte {
	return slices2(box("moof", bytes.Repeat([]byte{0}, 24)), box("mdat", bytes.Repeat([]byte{mark}, n)))
}

// logBuf collects warnings.
type logBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *logBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuf) count(s string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.b.String(), s)
}

// fakeBrowser hands each dial a pipe the test writes the stream into.
type fakeBrowser struct {
	streams chan *io.PipeWriter
	dials   int
	mu      sync.Mutex
}

func newFakeBrowser() *fakeBrowser { return &fakeBrowser{streams: make(chan *io.PipeWriter, 16)} }

func (fb *fakeBrowser) dial(ctx context.Context, profile string) (io.ReadCloser, error) {
	if profile == "" {
		return nil, errors.New("no profile")
	}
	pr, pw := io.Pipe()
	fb.mu.Lock()
	fb.dials++
	fb.mu.Unlock()
	fb.streams <- pw
	return pr, nil
}

// next waits for the recorder's next dial.
func (fb *fakeBrowser) next(t *testing.T) *io.PipeWriter {
	t.Helper()
	select {
	case w := <-fb.streams:
		return w
	case <-time.After(5 * time.Second):
		t.Fatal("the recorder didn't open a stream")
		return nil
	}
}

const (
	prof = "work"
	sess = "0123456789abcdef01234567"
)

func newTestRecorder(t *testing.T) (*Recorder, *Store, *fakeBrowser, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	log := slog.New(slog.NewTextHandler(lb, nil))
	st := NewStore(filepath.Join(t.TempDir(), "recordings"), log)
	st.Free = func() (int64, error) { return 1 << 40, nil }
	fb := newFakeBrowser()
	r := NewRecorder(st, fb.dial, log)
	r.retryAfter = 50 * time.Millisecond
	t.Cleanup(r.Close)
	return r, st, fb, lb
}

// waitFor polls cond for up to 5 s.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// tempSize is the size of the session's segment being written, -1 if none.
func tempSize(st *Store, n int) int64 {
	fi, err := os.Stat(filepath.Join(st.dir, prof, sess, segName(n)+tempSuffix))
	if err != nil {
		return -1
	}
	return fi.Size()
}

func segName(n int) string { return strings.Repeat("0", 5) + string(rune('0'+n)) + ".mp4" }

func readSegment(t *testing.T, st *Store, seg Segment) []byte {
	t.Helper()
	f, _, err := st.Open(prof, sess, seg.File)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestGateRecordsOnlyWhenEverythingHolds(t *testing.T) {
	states := map[string][2]bool{ // Running, NeedsHelp
		"starting": {false, false}, "running": {true, false}, "needs_help": {false, true}, "stopping": {false, false},
	}
	for state, rn := range states {
		for _, enabled := range []bool{false, true} {
			for _, human := range []bool{false, true} {
				for _, active := range []bool{false, true} {
					g := Gate{Enabled: enabled, Running: rn[0], NeedsHelp: rn[1], Human: human, AgentActive: active}
					got, why := g.Decide()
					want := enabled && state == "running" && !human && active
					if got != want {
						t.Errorf("%+v (%s): Decide() = %v, want %v", g, state, got, want)
					}
					if !got && why == "" {
						t.Errorf("%+v: no reason given for not recording", g)
					}
				}
			}
		}
	}
	if ok, why := (Gate{Enabled: true, Running: true, Human: true, AgentActive: true}).Decide(); ok || why != Person {
		t.Errorf("a person in control: %v %q, want false person", ok, why)
	}
	if ok, why := (Gate{Enabled: true, NeedsHelp: true, AgentActive: true}).Decide(); ok || why != Handoff {
		t.Errorf("a handoff: %v %q, want false handoff", ok, why)
	}
	if ok, why := (Gate{Enabled: true, Running: true, AgentActive: true, Paused: true}).Decide(); ok || why != Paused {
		t.Errorf("paused: %v %q, want false paused", ok, why)
	}
}

func TestBytesAfterTheGateClosesAreDropped(t *testing.T) {
	r, st, fb, _ := newTestRecorder(t)
	r.Set(sess, prof, true, "")
	w := fb.next(t)
	if _, err := w.Write(testInit); err != nil {
		t.Fatal(err)
	}
	before := 0
	for i := range 5 {
		f := frag('B', 400+i)
		if _, err := w.Write(f); err != nil {
			t.Fatal(err)
		}
		before += len(f)
		if i == 0 {
			before += len(testInit)
		}
		want := int64(before)
		waitFor(t, "the fragment on disk", func() bool { return tempSize(st, 1) == want })
	}
	// A person takes control: the gate closes. The browser keeps sending.
	r.Set(sess, prof, false, Person)
	if r.Allowed(sess) {
		t.Fatal("still allowed after Set(false)")
	}
	go func() {
		for range 20 {
			if _, err := w.Write(frag('A', 500)); err != nil {
				return
			}
		}
	}()
	waitFor(t, "the segment closed", func() bool { ix, err := st.Get(prof, sess); return err == nil && len(ix.Segments) == 1 })
	ix, _ := st.Get(prof, sess)
	b := readSegment(t, st, ix.Segments[0])
	if bytes.Contains(b, bytes.Repeat([]byte{'A'}, 16)) {
		t.Fatal("the segment holds bytes received after the gate closed")
	}
	if len(b) != before {
		t.Fatalf("segment is %d bytes, want the %d received before the gate closed", len(b), before)
	}
	if !bytes.HasPrefix(b, testInit) {
		t.Fatal("the segment doesn't start with the stream's ftyp and moov")
	}
	_ = w.Close()
}

func TestOversizeAndOverRateStreamsAreCut(t *testing.T) {
	t.Run("one box over the segment cap", func(t *testing.T) {
		r, st, fb, lb := newTestRecorder(t)
		r.maxSegment = 4 << 10
		r.Set(sess, prof, true, "")
		for attempt := range 2 {
			w := fb.next(t)
			go func() {
				_, _ = w.Write(testInit)
				_, _ = w.Write(frag('X', 8<<10))
			}()
			_ = attempt
		}
		r.Set(sess, prof, false, Idle)
		total, _ := st.Usage()
		if total != 0 {
			t.Fatalf("%d bytes written from a box over the cap", total)
		}
		if n := lb.count("over its cap"); n != 1 {
			t.Fatalf("%d warnings, want 1", n)
		}
	})
	t.Run("a segment reaching the cap ends and the next starts", func(t *testing.T) {
		r, st, fb, lb := newTestRecorder(t)
		r.maxSegment = 4 << 10
		r.Set(sess, prof, true, "")
		w := fb.next(t)
		go func() {
			_, _ = w.Write(testInit)
			for range 20 {
				if _, err := w.Write(frag('Y', 1000)); err != nil {
					return
				}
			}
		}()
		w2 := fb.next(t)
		waitFor(t, "the first segment", func() bool { ix, err := st.Get(prof, sess); return err == nil && len(ix.Segments) == 1 })
		ix, _ := st.Get(prof, sess)
		if ix.Segments[0].Bytes > 4<<10 {
			t.Fatalf("segment of %d bytes, over the %d cap", ix.Segments[0].Bytes, 4<<10)
		}
		if n := lb.count("reached its cap"); n != 1 {
			t.Fatalf("%d warnings, want 1", n)
		}
		_ = w2.Close()
	})
	t.Run("a stream over the per-minute cap", func(t *testing.T) {
		r, st, fb, lb := newTestRecorder(t)
		r.perMinute = 8 << 10
		r.Set(sess, prof, true, "")
		w := fb.next(t)
		go func() {
			_, _ = w.Write(testInit)
			for range 40 {
				if _, err := w.Write(frag('Z', 1000)); err != nil {
					return
				}
			}
		}()
		waitFor(t, "the cut segment", func() bool { ix, err := st.Get(prof, sess); return err == nil && len(ix.Segments) == 1 })
		ix, _ := st.Get(prof, sess)
		if total := ix.Bytes(); total > 8<<10 {
			t.Fatalf("%d bytes written in a minute, over the %d cap", total, 8<<10)
		}
		if n := lb.count("per-minute cap"); n != 1 {
			t.Fatalf("%d warnings, want 1", n)
		}
		if len(ix.Gaps) != 0 {
			// The lost stretch is saved with the next segment or at the end.
			t.Fatalf("gaps %v before the session went on", ix.Gaps)
		}
		r.End(sess)
		waitFor(t, "the lost gap", func() bool { ix, _ := st.Get(prof, sess); return len(ix.Gaps) > 0 })
		ix, _ = st.Get(prof, sess)
		if ix.Gaps[0].Reason != Lost || !ix.Gaps[0].Start.Equal(ix.Segments[0].End) {
			t.Fatalf("gaps %v, want lost from the cut segment's end", ix.Gaps)
		}
	})
}

func TestACrashMidSegmentLeavesNothingHalfWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	log := slog.New(slog.DiscardHandler)
	st := NewStore(dir, log)
	st.Free = func() (int64, error) { return 1 << 40, nil }
	d, err := st.create(prof, sess, slices2(testInit, frag('a', 100)))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.commit(time.Now(), time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	half, err := st.create(prof, sess, slices2(testInit, frag('b', 100)))
	if err != nil {
		t.Fatal(err)
	}
	_ = half.f.Close() // the orchestrator dies here, before the rename
	// A segment renamed but not yet in the index is no better.
	if err := os.WriteFile(filepath.Join(dir, prof, sess, "000009.mp4"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	st.Close()

	again := NewStore(dir, log)
	if err := again.Reconcile([]string{prof}); err != nil {
		t.Fatal(err)
	}
	ix, err := again.Get(prof, sess)
	if err != nil {
		t.Fatal(err)
	}
	if len(ix.Segments) != 1 || ix.Segments[0].File != "000001.mp4" {
		t.Fatalf("segments %+v, want only the whole one", ix.Segments)
	}
	names, _ := os.ReadDir(filepath.Join(dir, prof, sess))
	for _, n := range names {
		if n.Name() != "000001.mp4" && n.Name() != indexFile {
			t.Errorf("%s left in the session's folder", n.Name())
		}
	}
	// The next segment takes the next number, not one in use.
	d, err = again.create(prof, sess, slices2(testInit, frag('c', 10)))
	if err != nil {
		t.Fatal(err)
	}
	if d.name == "000001.mp4" {
		t.Fatal("a new segment took an existing name")
	}
	d.abort()
}

// feed sends a stream's head and n fragments, then waits until the
// segment being written holds them.
func feed(t *testing.T, st *Store, w *io.PipeWriter, seg, n int) {
	t.Helper()
	go func() {
		_, _ = w.Write(testInit)
		for range n {
			if _, err := w.Write(frag('f', 300)); err != nil {
				return
			}
		}
	}()
	want := int64(len(testInit) + n*len(frag('f', 300)))
	waitFor(t, "the fragments on disk", func() bool { return tempSize(st, seg) == want })
}

func TestTheIndexSaysWhyEachGapIsThere(t *testing.T) {
	r, st, fb, _ := newTestRecorder(t)
	r.stillAfter = 300 * time.Millisecond
	step := func(seg int, why Reason) {
		t.Helper()
		r.Set(sess, prof, true, "")
		feed(t, st, fb.next(t), seg, 3)
		r.Set(sess, prof, false, why)
		waitFor(t, "the segment closed", func() bool { ix, _ := st.Get(prof, sess); return len(ix.Segments) == seg })
		time.Sleep(20 * time.Millisecond)
	}
	step(1, Person)
	step(2, Handoff)
	step(3, Idle)
	// A still screen ends the segment by itself; the change after it starts the next.
	r.Set(sess, prof, true, "")
	w := fb.next(t)
	feed(t, st, w, 4, 1)
	go func() {
		for range 40 {
			if _, err := w.Write(frag('s', 10)); err != nil {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
	waitFor(t, "the still segment closed", func() bool { ix, _ := st.Get(prof, sess); return len(ix.Segments) == 4 })
	time.Sleep(50 * time.Millisecond)
	go func() { _, _ = w.Write(frag('m', 400)) }() // the screen changes
	feed(t, st, fb.next(t), 5, 2)
	// No room: the disk is short because of other data, which removing
	// recordings can't free. The next segment is lost, nothing recorded
	// goes, and the session goes on.
	r.Set(sess, prof, false, Idle)
	waitFor(t, "segment 5", func() bool { ix, _ := st.Get(prof, sess); return len(ix.Segments) == 5 })
	time.Sleep(20 * time.Millisecond)
	st.Free = func() (int64, error) { return 100 << 20, nil }
	r.Set(sess, prof, true, "")
	lostW := fb.next(t)
	go func() { _, _ = lostW.Write(testInit); _, _ = lostW.Write(frag('l', 300)) }()
	time.Sleep(100 * time.Millisecond)
	if ix, _ := st.Get(prof, sess); len(ix.Segments) != 5 {
		t.Fatalf("%d segments left after a refusal for room, want the 5 there were", len(ix.Segments))
	}
	st.Free = func() (int64, error) { return 1 << 40, nil }
	feed(t, st, fb.next(t), 6, 2)
	r.End(sess) // the browser stops
	time.Sleep(20 * time.Millisecond)
	waitFor(t, "the stop's gap", func() bool {
		ix, _ := st.Get(prof, sess)
		return len(ix.Gaps) > 0 && ix.Gaps[len(ix.Gaps)-1].Reason == Stopped
	})
	ix, _ := st.Get(prof, sess)
	var got []Reason
	for _, g := range ix.Gaps {
		got = append(got, g.Reason)
		if !g.End.After(g.Start) {
			t.Errorf("gap %+v doesn't move forward", g)
		}
	}
	want := []Reason{Person, Handoff, Idle, Still, Restart, Idle, Lost, Stopped}
	if strings.Join(reasons(got), ",") != strings.Join(reasons(want), ",") {
		t.Fatalf("gaps %v, want %v", got, want)
	}
	if len(ix.Segments) != 6 {
		t.Fatalf("%d segments, want 6", len(ix.Segments))
	}
	for i, seg := range ix.Segments {
		if i > 0 && seg.Start.Before(ix.Segments[i-1].End) {
			t.Errorf("segment %d starts before the one before it ends", i)
		}
		if !bytes.HasPrefix(readSegment(t, st, seg), testInit) {
			t.Errorf("segment %d doesn't start with its own head", i)
		}
	}
}

func reasons(rs []Reason) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r)
	}
	return out
}

// addSegment writes a whole segment with the given times.
func addSegment(t *testing.T, st *Store, profile, session string, size int, start, end time.Time) {
	t.Helper()
	d, err := st.create(profile, session, slices2(testInit, frag('d', size)))
	if err != nil {
		t.Fatalf("segment of %s: %v", profile, err)
	}
	if err := d.commit(start, end, nil); err != nil {
		t.Fatal(err)
	}
}

func TestRetentionRemovesOldSegments(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "recordings")
	st := NewStore(dir, slog.New(slog.DiscardHandler))
	st.Free = func() (int64, error) { return 1 << 40, nil }
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	st.now = func() time.Time { return now }
	oldSess, newSess := "aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb"
	addSegment(t, st, prof, oldSess, 10, now.Add(-9*24*time.Hour), now.Add(-9*24*time.Hour+time.Minute))
	addSegment(t, st, prof, oldSess, 10, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour+time.Minute))
	addSegment(t, st, prof, newSess, 10, now.Add(-8*24*time.Hour), now.Add(-8*24*time.Hour+time.Minute))
	addSegment(t, st, prof, newSess, 10, now.Add(-time.Hour), now.Add(-time.Hour+time.Minute))
	st.Sweep()
	if _, err := st.Get(prof, oldSess); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the old session is still listed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, prof, oldSess)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the emptied session's folder is still there: %v", err)
	}
	ix, err := st.Get(prof, newSess)
	if err != nil || len(ix.Segments) != 1 || ix.Segments[0].File != "000002.mp4" {
		t.Fatalf("the newer session: %+v, %v", ix.Segments, err)
	}
	// And at start, from what is on disk.
	now = now.Add(2 * time.Hour)
	st.Close()
	again := NewStore(dir, slog.New(slog.DiscardHandler))
	again.now = func() time.Time { return now.Add(8 * 24 * time.Hour) }
	if err := again.Reconcile([]string{prof}); err != nil {
		t.Fatal(err)
	}
	if got := again.Sessions(prof); len(got) != 0 {
		t.Fatalf("Reconcile kept %d sessions past the retention", len(got))
	}
}

func TestDiskCapTakesFromTheBiggestProfile(t *testing.T) {
	t.Run("cap", func(t *testing.T) {
		st := NewStore(filepath.Join(t.TempDir(), "recordings"), slog.New(slog.DiscardHandler))
		st.Free = func() (int64, error) { return 1 << 40, nil }
		seg := int64(len(testInit) + len(frag('d', 1000)))
		// Five segments and the two indexes.
		st.SetLimits(Limits{Retention: time.Hour * 24, Cap: 5*seg + 1000, KeepFree: 0})
		now := time.Now()
		quiet, busy := "quiet", "busy"
		qs, bs := "cccccccccccccccccccccccc", "dddddddddddddddddddddddd"
		addSegment(t, st, quiet, qs, 1000, now.Add(-time.Hour), now.Add(-time.Hour+time.Second))
		for i := range 4 {
			at := now.Add(time.Duration(i) * time.Minute)
			addSegment(t, st, busy, bs, 1000, at, at.Add(time.Second))
		}
		// At the cap: the busy profile gives up its oldest, not the quiet one's only.
		addSegment(t, st, busy, bs, 1000, now.Add(5*time.Minute), now.Add(5*time.Minute+time.Second))
		if got := st.Sessions(quiet); len(got) != 1 {
			t.Fatal("the quiet profile's only recording went")
		}
		ix, _ := st.Get(busy, bs)
		if len(ix.Segments) != 4 || ix.Segments[0].File != "000002.mp4" {
			t.Fatalf("busy segments %+v, want the oldest gone", ix.Segments)
		}
		if total, _ := st.Usage(); total > 5*seg+1000 {
			t.Fatalf("usage %d over the cap %d", total, 5*seg+1000)
		}
	})
	t.Run("free space floor", func(t *testing.T) {
		st := NewStore(filepath.Join(t.TempDir(), "recordings"), slog.New(slog.DiscardHandler))
		const disk = 1 << 20
		floor := int64(disk - 3000)
		st.Free = func() (int64, error) { total, _ := st.usageLocked(); return disk - total, nil }
		st.SetLimits(Limits{Retention: time.Hour, Cap: 1 << 30, KeepFree: floor})
		d, err := st.create(prof, sess, slices2(testInit, frag('d', 1000)))
		if err != nil {
			t.Fatal(err)
		}
		var werr error
		for werr == nil {
			werr = d.write(frag('e', 500))
			if free, _ := st.Free(); free < floor {
				t.Fatalf("free space %d crossed the floor %d", free, floor)
			}
		}
		if !errors.Is(werr, errNoRoom) {
			t.Fatal(werr)
		}
		if err := d.commit(time.Now(), time.Now(), nil); err != nil {
			t.Fatal(err)
		}
		// With the disk at the floor and nothing else to give up, nothing more is written.
		if _, err := st.create("other", "eeeeeeeeeeeeeeeeeeeeeeee", slices2(testInit, frag('d', 3000))); !errors.Is(err, errNoRoom) {
			t.Fatalf("create at the floor: %v", err)
		}
	})
	t.Run("a segment that can't fit is lost and the session goes on", func(t *testing.T) {
		r, st, fb, _ := newTestRecorder(t)
		st.SetLimits(Limits{Retention: time.Hour, Cap: 10, KeepFree: 0})
		r.Set(sess, prof, true, "")
		w := fb.next(t)
		go func() { _, _ = w.Write(testInit); _, _ = w.Write(frag('l', 300)) }()
		w2 := fb.next(t) // tried again later
		if total, _ := st.Usage(); total != 0 {
			t.Fatalf("%d bytes written without room", total)
		}
		st.SetLimits(DefaultLimits)
		feed(t, st, w2, 1, 2)
		r.End(sess)
		waitFor(t, "the recording", func() bool { ix, _ := st.Get(prof, sess); return len(ix.Gaps) > 0 })
		ix, _ := st.Get(prof, sess)
		if ix.Gaps[0].Reason != Lost {
			t.Fatalf("gaps %+v, want lost first", ix.Gaps)
		}
	})
}

func TestPathsStayInsideRecordings(t *testing.T) {
	base := t.TempDir()
	st := NewStore(filepath.Join(base, "recordings"), slog.New(slog.DiscardHandler))
	st.Free = func() (int64, error) { return 1 << 40, nil }
	addSegment(t, st, prof, sess, 10, time.Now(), time.Now())
	for _, p := range []string{"..", "../x", "a/b", "work/../..", ".deleted-work", ""} {
		if _, err := st.create(p, sess, testInit); err == nil {
			t.Errorf("create(%q) succeeded", p)
		}
		if _, err := st.Get(p, sess); err == nil {
			t.Errorf("Get(%q) succeeded", p)
		}
		if err := st.Delete(p, sess); err == nil {
			t.Errorf("Delete(%q) succeeded", p)
		}
	}
	for _, id := range []string{"..", "../" + sess, sess + "/..", "0123456789abcdef0123456/", ""} {
		if _, err := st.create(prof, id, testInit); err == nil {
			t.Errorf("create(session %q) succeeded", id)
		}
		if _, _, err := st.Open(prof, id, "000001.mp4"); err == nil {
			t.Errorf("Open(session %q) succeeded", id)
		}
	}
	for _, f := range []string{"../index.json", "index.json", "000001.mp4/..", "/etc/passwd", "000002.mp4"} {
		if _, _, err := st.Open(prof, sess, f); err == nil {
			t.Errorf("Open(file %q) succeeded", f)
		}
	}
	// Even a symlink planted inside can't lead out: os.Root.
	if err := os.Symlink(base, filepath.Join(base, "recordings", prof, sess, "000001.mp4.link")); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(base)
	if len(entries) != 1 {
		t.Fatalf("files outside recordings/: %v", entries)
	}
}
