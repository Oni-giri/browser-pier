package recording

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// Dialer opens a profile's recording stream: its browser's GET /record,
// over the control socket.
type Dialer func(ctx context.Context, profile string) (io.ReadCloser, error)

// Tuning, fixed in BrowserPier and smaller in tests.
const (
	// SegmentLength is the longest a segment lasts.
	SegmentLength = 30 * time.Second
	// stillAfter: a segment ends once the screen has been still this long.
	stillAfter = 3 * time.Second
	// stillBytes: a fragment whose mdat holds this many bytes or fewer is a
	// still screen (x264's skip frames are a few dozen bytes).
	stillBytes = 160
	// retryAfter is the wait after a cut or no room before trying again.
	retryAfter = 30 * time.Second
	// MaxSegments caps a session's segments: about 3.5 days of steady
	// work at one a SegmentLength. maxSegmentsPerMinute caps how fast
	// they come: a stream that ends at once, again and again, would
	// otherwise grow the index and the folder by the second.
	MaxSegments          = 10000
	maxSegmentsPerMinute = 20
	// shortStream: a stream that ends by itself sooner than this backs
	// the next one off, doubling up to retryAfter.
	shortStream = 3 * time.Second
)

// Recorder records the screens of the sessions the session manager says
// it may (Set), one goroutine and at most one stream per session.
type Recorder struct {
	store *Store
	dial  Dialer
	log   *slog.Logger
	now   func() time.Time

	segmentLength, stillAfter, retryAfter time.Duration
	maxSegment, perMinute                 int64
	maxSegments, segmentsPerMinute        int

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu       sync.Mutex
	sessions map[string]*session
	closed   bool
}

// NewRecorder returns a recorder writing to store.
func NewRecorder(store *Store, dial Dialer, log *slog.Logger) *Recorder {
	ctx, cancel := context.WithCancel(context.Background())
	return &Recorder{store: store, dial: dial, log: log, now: time.Now,
		segmentLength: SegmentLength, stillAfter: stillAfter, retryAfter: retryAfter,
		maxSegment: MaxSegment, perMinute: MaxPerMinute, maxSegments: MaxSegments, segmentsPerMinute: maxSegmentsPerMinute,
		ctx: ctx, cancel: cancel, sessions: map[string]*session{}}
}

// Store is where the recorder writes.
func (r *Recorder) Store() *Store { return r.store }

// session is one session's recording state.
type session struct {
	id, profile string
	kick        chan struct{}

	mu sync.Mutex
	// allow is the gate's answer; epoch changes each time it does. A
	// fragment is written only if it was whole before the answer went to
	// false: the check comes after it is received, under mu.
	allow  bool
	epoch  uint64
	why    Reason             // the gate's reason, while allow is false
	cancel context.CancelFunc // the stream's, if one is open
	ended  bool
	// Gaps: while no segment is open, the stretch since gapStart is one,
	// for gapWhy. gaps are those not yet saved.
	open     bool
	cutWhy   Reason // why the gate said no while a segment was open
	gapStart time.Time
	gapWhy   Reason
	gaps     []Gap
	// The current minute's bytes, against the per-minute cap.
	minute      time.Time
	minuteBytes int64
	minuteSegs  int
	warned      map[string]bool
	dialFails   int
	// segments counts the session's segments so far; shortStreams the
	// streams in a row that ended by themselves too soon.
	segments, shortStreams int
}

// Set tells the recorder whether session id, of profile, may be recorded
// now, and if not, why not. The session manager calls it with its lock
// held, at every change of the gate (Gate.Decide). It never waits on the
// stream or the disk: when allow is false, nothing received after it
// returns is written, the segment under way ends at the last fragment
// received before, and the pipeline is stopped afterwards.
func (r *Recorder) Set(id, profile string, allow bool, why Reason) {
	r.mu.Lock()
	s := r.sessions[id]
	if s == nil {
		if !allow || r.closed {
			r.mu.Unlock()
			return // nothing recorded yet, nothing to stop
		}
		s = &session{id: id, profile: profile, kick: make(chan struct{}, 1), warned: map[string]bool{}}
		r.sessions[id] = s
		r.wg.Add(1)
		go r.run(s)
	}
	r.mu.Unlock()

	s.mu.Lock()
	now := r.now()
	if allow != s.allow {
		s.allow = allow
		s.epoch++
	}
	if !allow {
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		s.noteWhyLocked(why, now)
	}
	s.mu.Unlock()
	s.poke()
}

// Losing reports whether session id may be recorded but nothing is kept
// for now: no room, a broken stream or a cap.
func (r *Recorder) Losing(id string) bool {
	r.mu.Lock()
	s := r.sessions[id]
	r.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allow && !s.open && s.gapWhy == Lost
}

// Allowed reports whether session id may be recorded now.
func (r *Recorder) Allowed(id string) bool {
	r.mu.Lock()
	s := r.sessions[id]
	r.mu.Unlock()
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allow
}

// End says session id is over. It doesn't wait: the segment under way is
// closed and the gaps saved in the background.
func (r *Recorder) End(id string) {
	r.mu.Lock()
	s := r.sessions[id]
	r.mu.Unlock()
	if s == nil {
		return
	}
	s.mu.Lock()
	s.ended = true
	if s.allow {
		s.allow = false
		s.epoch++
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.noteWhyLocked(Stopped, r.now())
	s.mu.Unlock()
	s.poke()
}

// Close stops every recording, closing the segments under way, and waits.
func (r *Recorder) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

func (s *session) poke() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// noteWhyLocked records the gate's reason; while no segment is open, a
// new reason starts a new gap.
func (s *session) noteWhyLocked(why Reason, now time.Time) {
	s.why = why
	if s.open && s.cutWhy == "" {
		s.cutWhy = why
	}
	if s.open || why == s.gapWhy {
		return
	}
	if !s.gapStart.IsZero() && s.gapWhy != "" && now.After(s.gapStart) {
		s.gaps = append(s.gaps, Gap{Start: s.gapStart, End: now, Reason: s.gapWhy})
	}
	s.gapStart, s.gapWhy = now, why
}

// accept reports whether a fragment received now, on a stream opened in
// epoch, may be written.
func (s *session) accept(epoch uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.allow && s.epoch == epoch
}

// begin opens a segment at start: the gap before it ends there.
func (s *session) begin(start time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.gapStart.IsZero() && s.gapWhy != "" && start.After(s.gapStart) {
		s.gaps = append(s.gaps, Gap{Start: s.gapStart, End: start, Reason: s.gapWhy})
	}
	s.gapStart, s.gapWhy = time.Time{}, ""
	s.open = true
}

// takeGaps returns the gaps not yet saved.
func (s *session) takeGaps() []Gap {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.gaps
	s.gaps = nil
	return g
}

// closed ends the open segment at end. why is the gap's reason after it;
// "" for the gate's, if it said no, else none (a new segment follows).
func (s *session) closed(end time.Time, why Reason) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if why == "" {
		why = s.cutWhy
	}
	if why == "" && !s.allow {
		why = s.why
	}
	s.open, s.cutWhy = false, ""
	s.gapStart, s.gapWhy = end, why
}

// lost marks a stretch from at with no segment open as Lost.
func (s *session) lost(at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.open && s.gapWhy != Lost {
		if !s.gapStart.IsZero() && s.gapWhy != "" && at.After(s.gapStart) {
			s.gaps = append(s.gaps, Gap{Start: s.gapStart, End: at, Reason: s.gapWhy})
		}
		s.gapStart, s.gapWhy = at, Lost
	}
}

// countMinute counts n bytes against the per-minute cap.
func (s *session) countMinute(at time.Time, n, limit int64) bool {
	if at.Sub(s.minute) >= time.Minute {
		s.minute, s.minuteBytes, s.minuteSegs = at, 0, 0
	}
	if s.minuteBytes+n > limit {
		return false
	}
	s.minuteBytes += n
	return true
}

func (r *Recorder) warnOnce(s *session, kind, msg string, args ...any) {
	if s.warned[kind] {
		return
	}
	s.warned[kind] = true
	r.log.Warn(msg, append([]any{"session", s.id, "profile", s.profile}, args...)...)
}

// run is a session's loop: a stream at a time while the gate says yes.
func (r *Recorder) run(s *session) {
	defer r.wg.Done()
	wait := time.Duration(0)
	for {
		ctx, epoch, ok := r.await(s, wait)
		if !ok {
			r.finish(s)
			return
		}
		wait = r.stream(ctx, s, epoch)
	}
}

// await waits until the session may be recorded and wait has passed, and
// returns the context of the stream to open: Set cancels it.
func (r *Recorder) await(s *session, wait time.Duration) (context.Context, uint64, bool) {
	var timer <-chan time.Time
	if wait > 0 {
		t := time.NewTimer(wait)
		defer t.Stop()
		timer = t.C
	}
	for {
		s.mu.Lock()
		if s.ended || r.ctx.Err() != nil {
			s.mu.Unlock()
			return nil, 0, false
		}
		if s.allow && timer == nil {
			ctx, cancel := context.WithCancel(r.ctx)
			s.cancel = cancel
			epoch := s.epoch
			s.mu.Unlock()
			return ctx, epoch, true
		}
		s.mu.Unlock()
		select {
		case <-s.kick:
		case <-timer:
			timer = nil
		case <-r.ctx.Done():
		}
	}
}

// finish saves the last gap as the session ends, and forgets it.
func (r *Recorder) finish(s *session) {
	s.mu.Lock()
	now := r.now()
	if !s.open && !s.gapStart.IsZero() && s.gapWhy != "" && now.After(s.gapStart) {
		s.gaps = append(s.gaps, Gap{Start: s.gapStart, End: now, Reason: s.gapWhy})
	}
	s.gapStart, s.gapWhy = time.Time{}, ""
	gaps := s.gaps
	s.gaps = nil
	ended := s.ended
	s.mu.Unlock()
	r.store.addGaps(s.profile, s.id, gaps)
	if ended || r.ctx.Err() != nil {
		r.mu.Lock()
		if r.sessions[s.id] == s {
			delete(r.sessions, s.id)
		}
		r.mu.Unlock()
	}
}

// stream reads one GET /record: at most one segment, from its first
// fragment (a keyframe) to the end of the stream, the gate saying no, a
// still screen, a cap or SegmentLength. It returns how long to wait
// before the next.
func (r *Recorder) stream(ctx context.Context, s *session, epoch uint64) time.Duration {
	defer func() {
		s.mu.Lock()
		if s.cancel != nil {
			s.cancel()
			s.cancel = nil
		}
		s.mu.Unlock()
	}()
	body, err := r.dial(ctx, s.profile)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		// The pipeline of the last stream may still be on its way out
		// (409): try again at once, every 50 ms for a second, then back off.
		s.dialFails++
		if s.dialFails <= 20 {
			return 50 * time.Millisecond
		}
		r.warnOnce(s, "dial", "can't open the browser's recording stream; will keep trying", "err", err)
		return min(250*time.Millisecond<<min(s.dialFails-20, 5), 5*time.Second)
	}
	s.dialFails = 0
	opened := r.now()
	defer func() { _ = body.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = body.Close() })
	defer stop()

	init, err := readInit(body)
	if err != nil {
		if ctx.Err() != nil {
			return 0
		}
		r.warnOnce(s, "stream", "the browser's recording stream is broken; will try again later", "err", err)
		s.lost(r.now())
		return r.retryAfter
	}
	var (
		d           *draft
		start, last time.Time
		stillSince  time.Time
		closedStill bool
	)
	// end closes the segment under way, if any, at the last fragment.
	end := func(why Reason) {
		if d == nil {
			return
		}
		if err := d.commit(start, last, s.takeGaps()); err != nil {
			r.warnOnce(s, "commit", "saving a recording segment", "err", err)
		}
		s.closed(last, why)
		d = nil
	}
	for {
		frag, err := readFragment(body, r.maxSegment-int64(len(init)))
		at := r.now()
		switch {
		case err != nil && ctx.Err() != nil:
			end("") // the gate said no, the session ended or BrowserPier is closing
			return 0
		case errors.Is(err, errOverCap):
			end(Lost)
			r.warnOnce(s, "cap", "a recording stream went over its cap; it was cut and will be tried again later", "err", err)
			s.lost(at)
			return r.retryAfter
		case err != nil:
			end(Lost)
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				r.warnOnce(s, "stream", "the browser's recording stream is broken; will try again later", "err", err)
				s.lost(at)
				return r.retryAfter
			}
			s.lost(at)
			// The pipeline ended by itself. One that ends at once, again
			// and again, is backed off.
			if at.Sub(opened) >= shortStream {
				s.shortStreams = 0
				return time.Second
			}
			s.shortStreams++
			return min(time.Second<<min(s.shortStreams, 6), r.retryAfter)
		}
		// Checked once the fragment is whole: one that ended after the
		// gate said no is dropped, and the segment ends before it.
		if !s.accept(epoch) {
			end("")
			return 0
		}
		still := frag.payload <= stillBytes
		switch {
		case !still:
			stillSince = time.Time{}
		case stillSince.IsZero():
			stillSince = at
		}
		if d == nil {
			if closedStill {
				if still {
					continue // nothing to keep
				}
				return 0 // the screen changed: a new segment, from a new keyframe
			}
			// The stream's first fragment, which holds its keyframe.
			if s.segments >= r.maxSegments {
				r.warnOnce(s, "segments", "a session reached its cap on recording segments; nothing more of it is recorded", "cap", r.maxSegments)
				s.lost(at)
				return time.Hour
			}
			if !s.countSegmentLocked(at, r.segmentsPerMinute) {
				r.warnOnce(s, "segment-rate", "a recording stream started too many segments in a minute; it was cut and will be tried again later")
				s.lost(at)
				return time.Minute
			}
			if !s.countMinuteLocked(at, int64(len(init)+len(frag.data)), r.perMinute) {
				r.warnOnce(s, "minute", "a recording stream went over its per-minute cap; it was cut and will be tried again later")
				s.lost(at)
				return time.Minute
			}
			d, err = r.store.create(s.profile, s.id, slices.Concat(init, frag.data))
			if err != nil {
				if !errors.Is(err, errNoRoom) {
					r.warnOnce(s, "create", "starting a recording segment", "err", err)
				}
				s.lost(at)
				return r.retryAfter
			}
			start, last = at, at
			s.segments++
			s.begin(at)
			continue
		}
		if at.Sub(start) >= r.segmentLength || d.bytes+int64(len(frag.data)) > r.maxSegment {
			if at.Sub(start) < r.segmentLength {
				r.warnOnce(s, "segment", "a recording segment reached its cap before its time; the next one starts at once", "cap", r.maxSegment)
			}
			// The next segment starts at once, from a new keyframe: what
			// comes between (this fragment, and the new pipeline's start)
			// is a Restart gap.
			end(Restart)
			return 0
		}
		if !s.countMinuteLocked(at, int64(len(frag.data)), r.perMinute) {
			end(Lost)
			r.warnOnce(s, "minute", "a recording stream went over its per-minute cap; it was cut and will be tried again later")
			s.lost(at)
			return time.Minute
		}
		if err := d.write(frag.data); err != nil {
			end(Lost)
			if !errors.Is(err, errNoRoom) {
				r.warnOnce(s, "write", "writing a recording segment", "err", err)
			}
			s.lost(at)
			return r.retryAfter
		}
		last = at
		if !stillSince.IsZero() && at.Sub(stillSince) >= r.stillAfter {
			end(Still)
			closedStill = true
		}
	}
}

// countSegmentLocked counts a new segment against the per-minute cap.
func (s *session) countSegmentLocked(at time.Time, limit int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if at.Sub(s.minute) >= time.Minute {
		s.minute, s.minuteBytes, s.minuteSegs = at, 0, 0
	}
	if s.minuteSegs >= limit {
		return false
	}
	s.minuteSegs++
	return true
}

func (s *session) countMinuteLocked(at time.Time, n, limit int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.countMinute(at, n, limit)
}
