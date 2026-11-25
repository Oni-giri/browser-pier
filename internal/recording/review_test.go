package recording

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestNoRoomForOtherReasonsRemovesNothing: with the disk under the floor
// because of other data, removing recordings can't make room, so none is
// removed, the write is refused, and that is logged once.
func TestNoRoomForOtherReasonsRemovesNothing(t *testing.T) {
	lb := &logBuf{}
	st := NewStore(filepath.Join(t.TempDir(), "recordings"), slog.New(slog.NewTextHandler(lb, nil)))
	st.Free = func() (int64, error) { return 1 << 40, nil }
	now := time.Now()
	ids := []string{"aaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbb", "cccccccccccccccccccccccc"}
	for i, p := range []string{"mail", "bank", "shop"} {
		for k := range 3 {
			at := now.Add(time.Duration(i*3+k) * time.Minute)
			addSegment(t, st, p, ids[i], 1000, at, at.Add(time.Second))
		}
	}
	before, _ := st.Usage()
	st.Free = func() (int64, error) { return 500 << 20, nil } // under the 1 GiB floor, whatever recordings hold
	for range 3 {
		if _, err := st.create("news", sess, slices2(testInit, frag('n', 100))); !errors.Is(err, errNoRoom) {
			t.Fatalf("create under the floor: %v", err)
		}
	}
	if after, _ := st.Usage(); after != before {
		t.Fatalf("usage %d after refused writes, want the %d there was", after, before)
	}
	for _, p := range []string{"mail", "bank", "shop"} {
		if got := st.Sessions(p); len(got) != 1 || len(got[0].Segments) != 3 {
			t.Fatalf("%s lost recordings to a write that could never fit: %+v", p, got)
		}
	}
	if n := lb.count("no room for a recording segment"); n != 1 {
		t.Fatalf("%d warnings, want 1", n)
	}
	// Where removing recordings does make room, they give way, oldest first.
	var total int64
	st.Free = func() (int64, error) { t, _ := st.usageLocked(); return KeepFree + 4000 - (t - total), nil }
	total = before
	if _, err := st.create("news", sess, slices2(testInit, frag('n', 5000))); err != nil {
		t.Fatalf("create where recordings can give way: %v", err)
	}
}

func TestDeletingALiveRecordingDropsTheSegmentUnderWay(t *testing.T) {
	for _, all := range []bool{false, true} {
		st := NewStore(filepath.Join(t.TempDir(), "recordings"), slog.New(slog.DiscardHandler))
		st.Free = func() (int64, error) { return 1 << 40, nil }
		addSegment(t, st, prof, sess, 100, time.Now().Add(-time.Minute), time.Now().Add(-30*time.Second))
		d, err := st.create(prof, sess, slices2(testInit, frag('u', 100)))
		if err != nil {
			t.Fatal(err)
		}
		if all {
			err = st.DeleteAll(prof)
		} else {
			err = st.Delete(prof, sess)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := d.write(frag('v', 100)); err != nil {
			t.Fatal(err)
		}
		if err := d.commit(time.Now().Add(-10*time.Second), time.Now(), nil); err != nil {
			t.Fatal(err)
		}
		if ix, err := st.Get(prof, sess); !errors.Is(err, ErrNotFound) {
			t.Fatalf("all %v: the deleted recording came back with %+v", all, ix.Segments)
		}
		if total, _ := st.Usage(); total != 0 {
			t.Fatalf("all %v: %d bytes left", all, total)
		}
		if _, err := os.Stat(filepath.Join(st.dir, prof, sess)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("all %v: the session's folder is still there: %v", all, err)
		}
		// What the session records afterwards is a new recording.
		addSegment(t, st, prof, sess, 100, time.Now(), time.Now())
		if ix, err := st.Get(prof, sess); err != nil || len(ix.Segments) != 1 {
			t.Fatalf("all %v: after the delete: %+v, %v", all, ix.Segments, err)
		}
	}
}

// hostileBrowser answers every dial at once with a stream it chose.
type hostileBrowser struct {
	mu     sync.Mutex
	dials  int
	stream func() []byte
}

func (h *hostileBrowser) dial(context.Context, string) (io.ReadCloser, error) {
	h.mu.Lock()
	h.dials++
	h.mu.Unlock()
	return io.NopCloser(bytes.NewReader(h.stream())), nil
}

func (h *hostileBrowser) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.dials
}

func hostileRecorder(t *testing.T, h *hostileBrowser) (*Recorder, *Store, *logBuf) {
	t.Helper()
	lb := &logBuf{}
	log := slog.New(slog.NewTextHandler(lb, nil))
	st := NewStore(filepath.Join(t.TempDir(), "recordings"), log)
	st.Free = func() (int64, error) { return 1 << 40, nil }
	r := NewRecorder(st, h.dial, log)
	t.Cleanup(r.Close)
	return r, st, lb
}

func TestAHostileStreamCantFloodTheIndex(t *testing.T) {
	oneFrame := func() []byte { return slices2(testInit, frag('h', 500)) } // a segment, then the end
	t.Run("streams that end at once back off", func(t *testing.T) {
		h := &hostileBrowser{stream: oneFrame}
		r, st, _ := hostileRecorder(t, h)
		r.Set(sess, prof, true, "")
		time.Sleep(3 * time.Second)
		r.Set(sess, prof, false, Idle)
		ix, _ := st.Get(prof, sess)
		if n := h.count(); n > 4 {
			t.Fatalf("%d streams in 3 s from a browser whose streams end at once, want a back-off", n)
		}
		t.Logf("%d streams, %d segments in 3 s", h.count(), len(ix.Segments))
	})
	t.Run("segments a minute", func(t *testing.T) {
		h := &hostileBrowser{stream: oneFrame}
		r, st, lb := hostileRecorder(t, h)
		r.retryAfter = 10 * time.Millisecond // no back-off: only the cap holds it
		r.segmentsPerMinute = 5
		r.Set(sess, prof, true, "")
		waitFor(t, "the cap's warning", func() bool { return lb.count("too many segments in a minute") > 0 })
		time.Sleep(200 * time.Millisecond)
		r.Set(sess, prof, false, Idle)
		if ix, _ := st.Get(prof, sess); len(ix.Segments) != 5 {
			t.Fatalf("%d segments, want the cap's 5", len(ix.Segments))
		}
		if n := lb.count("too many segments in a minute"); n != 1 {
			t.Fatalf("%d warnings, want 1", n)
		}
	})
	t.Run("segments a session", func(t *testing.T) {
		h := &hostileBrowser{stream: oneFrame}
		r, st, lb := hostileRecorder(t, h)
		r.retryAfter = 10 * time.Millisecond
		r.maxSegments = 3
		r.Set(sess, prof, true, "")
		waitFor(t, "the cap's warning", func() bool { return lb.count("cap on recording segments") > 0 })
		time.Sleep(200 * time.Millisecond)
		if ix, _ := st.Get(prof, sess); len(ix.Segments) != 3 {
			t.Fatalf("%d segments, want the cap's 3", len(ix.Segments))
		}
	})
	t.Run("boxes to skip", func(t *testing.T) {
		h := &hostileBrowser{stream: func() []byte {
			b := slices2(testInit, nil)
			for range 50 {
				b = append(b, box("free", make([]byte, 1000))...)
			}
			return b
		}}
		r, st, lb := hostileRecorder(t, h)
		r.Set(sess, prof, true, "")
		waitFor(t, "the cut", func() bool { return lb.count("isn't the MP4 it should be") > 0 })
		if total, _ := st.Usage(); total != 0 {
			t.Fatalf("%d bytes kept from a stream of padding", total)
		}
	})
	t.Run("the index counts against the cap", func(t *testing.T) {
		st := NewStore(filepath.Join(t.TempDir(), "recordings"), slog.New(slog.DiscardHandler))
		st.Free = func() (int64, error) { return 1 << 40, nil }
		addSegment(t, st, prof, sess, 100, time.Now(), time.Now())
		ix, _ := st.Get(prof, sess)
		fi, err := os.Stat(filepath.Join(st.dir, prof, sess, indexFile))
		if err != nil {
			t.Fatal(err)
		}
		if total, _ := st.Usage(); total != ix.Bytes()+fi.Size() {
			t.Fatalf("usage %d, want the segment's %d and the index's %d", total, ix.Bytes(), fi.Size())
		}
	})
}

func TestALowerCapAppliesAtTheNextSweep(t *testing.T) {
	st := NewStore(filepath.Join(t.TempDir(), "recordings"), slog.New(slog.DiscardHandler))
	st.Free = func() (int64, error) { return 1 << 40, nil }
	for k := range 5 {
		at := time.Now().Add(time.Duration(k) * time.Minute)
		addSegment(t, st, prof, sess, 2000, at, at.Add(time.Second))
	}
	st.SetLimits(Limits{Retention: time.Hour, Cap: 5000, KeepFree: 0})
	st.Sweep()
	if total, _ := st.Usage(); total > 5000 {
		t.Fatalf("usage %d over the lowered cap", total)
	}
	if ix, _ := st.Get(prof, sess); len(ix.Segments) == 0 || ix.Segments[len(ix.Segments)-1].File != "000005.mp4" {
		t.Fatalf("segments %+v, want the newest kept", ix.Segments)
	}
}
