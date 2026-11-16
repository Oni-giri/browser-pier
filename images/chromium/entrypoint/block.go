package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/Oni-giri/browser-pier/internal/domainset"
)

// errBlocked is a host on the profile's ad and tracker list (PROJ-119).
var errBlocked = errors.New("host on the ad and tracker list")

// errExemptFull is an exemption past maxExempt.
var errExemptFull = errors.New("the session exempts no more hosts from the ad and tracker list")

// maxExempt caps the hosts a session can exempt from the list. Only hosts
// the list blocks take a place.
const maxExempt = 256

// blocker refuses connections to hosts on the ad and tracker list, unless
// the built-in allowlist or the session's exemptions say otherwise. It
// only ever sees host names: IP addresses are the egress policy's alone.
// A nil blocker blocks nothing.
type blocker struct {
	set     *domainset.Set
	on      atomic.Bool
	blocked atomic.Int64 // connections refused for the list

	mu     sync.Mutex
	exempt []string // hosts the agent opened directly, normalised
	full   bool     // an exemption was refused for the cap (logged once)

	log *slog.Logger // nil: slog.Default()
}

// readBlockFile loads the list the orchestrator left in the control
// folder. A missing file means no list, which is logged by the caller.
func readBlockFile(path string) (*domainset.Set, error) {
	f, err := os.Open(path) //nolint:gosec // the container's own config
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return domainset.Read(f)
}

func newBlocker(set *domainset.Set) *blocker {
	b := &blocker{set: set}
	b.on.Store(set.Len() > 0)
	return b
}

// blocks reports whether a connection to host must be refused for the list.
func (b *blocker) blocks(host string) bool {
	if b == nil || !b.on.Load() {
		return false
	}
	h, ok := domainset.Normalize(host)
	if !ok || domainset.Allowed(h) || b.exempted(h) {
		return false
	}
	return b.set.Matches(h)
}

// exempted reports whether h, or a parent domain of it, was opened
// directly in this session.
func (b *blocker) exempted(h string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, e := range b.exempt {
		if h == e || (len(h) > len(e) && h[len(h)-len(e)-1] == '.' && h[len(h)-len(e):] == e) {
			return true
		}
	}
	return false
}

// exemptHost lets host and its subdomains through for the rest of the
// container's life: the agent opened a page there directly. A host the
// list doesn't block (or the allowlist lets through, or with blocking
// off) needs nothing and takes none of the maxExempt places. A public
// suffix (co.uk, github.io) is refused: it would let through every site
// under it. Past the cap it answers errExemptFull, logged once.
func (b *blocker) exemptHost(host string) error {
	h, ok := domainset.Normalize(host)
	if !ok {
		return fmt.Errorf("%q isn't a host name", host)
	}
	if domainset.PublicSuffix(h) {
		return fmt.Errorf("%q is a public suffix", h)
	}
	if !b.on.Load() || domainset.Allowed(h) || !b.set.Matches(h) {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if slices.Contains(b.exempt, h) {
		return nil
	}
	if len(b.exempt) >= maxExempt {
		if !b.full {
			b.full = true
			log := b.log
			if log == nil {
				log = slog.Default()
			}
			log.Warn("an agent opened more listed hosts than can be exempted from the ad and tracker list; the rest stay blocked", "max", maxExempt)
		}
		return errExemptFull
	}
	b.exempt = append(b.exempt, h)
	return nil
}

// register serves the blocker's controls on the control socket, which
// only the orchestrator reaches:
//
//	POST /proxy/blocking/off   stop blocking for the rest of the container's life; it can't be turned back on
//	POST /proxy/exempt         {"host": "..."}: let that host and its subdomains through (400 for a public suffix, 409 past the cap)
func (b *blocker) register(mux *http.ServeMux) {
	mux.HandleFunc("POST /proxy/blocking/off", func(w http.ResponseWriter, _ *http.Request) {
		b.on.Store(false)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /proxy/exempt", func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Host string `json:"host"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&in); err != nil {
			http.Error(w, "want {\"host\": \"...\"}", http.StatusBadRequest)
			return
		}
		if err := b.exemptHost(in.Host); errors.Is(err, errExemptFull) {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		} else if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
