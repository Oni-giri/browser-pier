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
// The orchestrator only lets agents back in once the fence is in place,
// and a new takeover's number is always above every fence: within a
// session the orchestrator counts up, and a new session is a new
// container, whose fence starts at 0. After the orchestrator restarts,
// its lift fences "all", every takeover seen so far, and it numbers the
// next takeovers above the fence answered.
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
	// seen is the highest takeover any request has named, as it arrived.
	seen atomic.Uint64
}

// takeover reads the takeover a request names, as it arrives, before
// anything else is done with it. person is false for one that names none
// (an agent's input, the orchestrator's own calls).
func (f *takeoverFence) takeover(r *http.Request) (gen uint64, person bool, err error) {
	s := r.Header.Get(takeoverHeader)
	if s == "" {
		return 0, false, nil
	}
	gen, err = strconv.ParseUint(s, 10, 64)
	if err != nil || gen == 0 {
		return 0, false, errors.New(takeoverHeader + ": want a takeover number")
	}
	for {
		old := f.seen.Load()
		if gen <= old || f.seen.CompareAndSwap(old, gen) {
			break
		}
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
	return f.mu.RUnlock, true
}

// raise puts the fence at n at least, or with all, at every takeover seen
// so far too, once the persons' actions under way are over, and returns
// it.
func (f *takeoverFence) raise(n uint64, all bool) uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	if all {
		n = max(n, f.seen.Load())
	}
	f.n = max(f.n, n)
	return f.n
}

// fenceRequest reads a lift's fenceHeader: a takeover number or "all".
// set is false when there is none.
func fenceRequest(r *http.Request) (n uint64, all, set bool, err error) {
	s := r.Header.Get(fenceHeader)
	switch s {
	case "":
		return 0, false, false, nil
	case "all":
		return 0, true, true, nil
	}
	n, err = strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false, false, errors.New(fenceHeader + `: want a takeover number or "all"`)
	}
	return n, false, true, nil
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
