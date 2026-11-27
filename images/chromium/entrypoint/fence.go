package main

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
)

// A person's control of the browser is a takeover, numbered by the
// orchestrator (its generation). Everything the person's live view sends
// names its takeover in takeoverHeader: live input, typing, and the
// clipboard box's read, set and paste. When the takeover ends, the
// orchestrator's lift (POST /input/release) carries fenceHeader: from
// then on, anything from that takeover or an earlier one is refused, with
// 409, however long it was held up on its way here (PROJ-196). Agents'
// input names no takeover and is never fenced.
//
// The orchestrator only lets agents back in once the fence is in place
// (a 200 answer giving it), and a new takeover's number is always above
// every fence: within a session the orchestrator counts up, saving each
// number before handing it out, and a new session is a new container,
// whose fence starts at 0. After the orchestrator restarts, its lift
// fences the highest number it saved, so a request of the BrowserPier
// before it is refused however late its handler runs, and it numbers
// the next takeovers above it.
//
// The hand-back's cleanup (the lift, the clipboard's emptying) names the
// takeover it is for, and does nothing to a later takeover that has
// begun meanwhile (a call held up past its time): see cleanup.
const (
	takeoverHeader = "X-BrowserPier-Takeover" // sessions.TakeoverHeader
	fenceHeader    = "X-BrowserPier-Fence"    // sessions.FenceHeader
)

// errFenced is the refusal of an operation from a takeover that ended.
var errFenced = errors.New("this person's control of the browser has ended")

// takeoverFence refuses the operations of takeovers that have ended.
type takeoverFence struct {
	// mu is held for reading across each person's action, from the
	// check against the fence to the action's end, and for writing to
	// raise the fence: an action either ran wholly before the fence went
	// up (and the lift and emptying that follow undo it) or is refused.
	mu sync.RWMutex
	n  uint64 // the fence: takeovers up to n have ended
	// admitted is the highest takeover an action was admitted for.
	admitted atomic.Uint64
}

// takeover reads the takeover a request names. person is false for one
// that names none (an agent's input, the orchestrator's own calls).
func (f *takeoverFence) takeover(r *http.Request) (gen uint64, person bool, err error) {
	s := r.Header.Get(takeoverHeader)
	if s == "" {
		return 0, false, nil
	}
	gen, err = strconv.ParseUint(s, 10, 64)
	if err != nil || gen == 0 {
		return 0, false, errors.New(takeoverHeader + ": want a takeover number")
	}
	return gen, true, nil
}

// admit is called right before a person's action, once everything else
// about the request has been checked: ok is false if their takeover has
// ended. Call done once the action is over (raise waits for it). An
// operation naming no takeover is always admitted, and holds nothing.
func (f *takeoverFence) admit(gen uint64, person bool) (done func(), ok bool) {
	if !person || f == nil {
		return func() {}, true
	}
	f.mu.RLock()
	if gen <= f.n {
		f.mu.RUnlock()
		return nil, false
	}
	for {
		old := f.admitted.Load()
		if gen <= old || f.admitted.CompareAndSwap(old, gen) {
			break
		}
	}
	return f.mu.RUnlock, true
}

// cleanup runs a hand-back's cleanup for takeover n (do), in one step
// under the fence's write lock, so once the persons' actions under way
// are over and with none starting meanwhile: it puts the fence at n at
// least first, then calls do with stale set if a takeover above n has
// already been admitted, whose input, keys and clipboard do must leave
// alone (a cleanup held up past its time). It returns the fence.
func (f *takeoverFence) cleanup(n uint64, do func(stale bool) error) (uint64, error) {
	if f == nil {
		return n, do(false)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n = max(f.n, n)
	return f.n, do(f.admitted.Load() > n)
}

// raise puts the fence at n at least, once the persons' actions under way
// are over, and returns it.
func (f *takeoverFence) raise(n uint64) uint64 {
	fence, _ := f.cleanup(n, func(bool) error { return nil })
	return fence
}

// fenceRequest reads a cleanup's fenceHeader: the takeover it is for. set
// is false when there is none.
func fenceRequest(r *http.Request) (n uint64, set bool, err error) {
	s := r.Header.Get(fenceHeader)
	if s == "" {
		return 0, false, nil
	}
	n, err = strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, errors.New(fenceHeader + ": want a takeover number")
	}
	return n, true, nil
}

// refuseFenced answers an operation from a takeover that ended.
func refuseFenced(w http.ResponseWriter) {
	http.Error(w, errFenced.Error(), http.StatusConflict)
}

// answerFence answers a fenced lift with the fence now in place.
func answerFence(w http.ResponseWriter, n uint64) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]uint64{"fence": n})
}
