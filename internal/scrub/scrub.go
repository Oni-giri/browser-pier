// Package scrub hides secrets BrowserPier itself typed into a page from
// everything it hands back to agents (PROJ-125). A vault fill (PROJ-57)
// types a value the agent never sees; a page may echo it anywhere (body
// text, a title, a GET form's URL, an error), so every answer goes through
// a Scrubber that replaces each such value, in every encoding a page or an
// encoder could give it, with Hidden.
//
// The values live in a Registry, in memory only: nothing here logs,
// stores or formats them, and no error or panic message carries one.
package scrub

import (
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

// Hidden replaces a secret.
const Hidden = "<hidden>"

// MinRunes is the shortest secret scrubbed: shorter ones would hide
// ordinary text ("1234" in a price) for no real protection.
const MinRunes = 4

// Scrubber replaces a fixed set of secrets. It is immutable and safe for
// concurrent use; a nil *Scrubber hides nothing.
type Scrubber struct {
	m       *matcher
	secrets []string // the values it was built from, for Union
}

// New builds a Scrubber for secrets, ignoring those under MinRunes
// characters. It returns nil when none is left.
func New(secrets []string) *Scrubber {
	var pats, kept []string
	seen := map[string]bool{}
	for _, s := range secrets {
		if utf8.RuneCountInString(s) < MinRunes {
			continue
		}
		kept = append(kept, s)
		add := func(p string) {
			if !seen[p] {
				seen[p] = true
				pats = append(pats, p)
			}
		}
		for _, f := range forms(s) {
			p := normalize(f)
			add(p)
			if loneBackslash(f) {
				// The text's next byte pairs with it if it is punctuation:
				// "ab\" then "." reads as "ab.".
				for c := byte('!'); c < 0x7F; c++ {
					if isPunct(c) {
						add(p[:len(p)-1] + string(c))
					}
				}
			}
		}
	}
	if len(pats) == 0 {
		return nil
	}
	return &Scrubber{m: newMatcher(pats), secrets: kept}
}

// String replaces every secret in text with Hidden. Where matches overlap
// (one secret inside another, or two touching), their union is hidden
// whole, so no part of either is left. Text with no secret comes back as
// it was, without allocating.
func (s *Scrubber) String(text string) string {
	if s == nil || text == "" {
		return text
	}
	spans := s.m.find(text)
	if len(spans) == 0 {
		return text
	}
	var b strings.Builder
	b.Grow(len(text))
	at := 0
	for _, sp := range spans {
		b.WriteString(text[at:sp.start])
		b.WriteString(Hidden)
		at = sp.end
	}
	b.WriteString(text[at:])
	return b.String()
}

// Registry holds the secrets typed into each owner's page (a session's
// ID), in memory only, and caches a Scrubber per set of owners, rebuilt
// only when a secret is added or an owner forgotten. Building one takes
// time and memory in the secrets' forms, so it happens outside the
// registry's lock: nothing else waits on it.
type Registry struct {
	mu      sync.Mutex
	secrets map[string][]string
	cache   map[string]*Scrubber
	version uint64 // bumped by every change to secrets
}

// NewRegistry makes an empty registry.
func NewRegistry() *Registry {
	return &Registry{secrets: map[string][]string{}, cache: map[string]*Scrubber{}}
}

// maxCached bounds the cache of Scrubbers by owner set: a Scrubber for a
// few long secrets can take a few megabytes.
const maxCached = 8

// build makes a Scrubber; tests replace it to watch the locks.
var build = New

// Add registers a secret for owner. Values under MinRunes characters are
// ignored, as Scrubber ignores them; adding one twice is harmless.
func (r *Registry) Add(owner, value string) {
	if utf8.RuneCountInString(value) < MinRunes {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if slices.Contains(r.secrets[owner], value) {
		return
	}
	r.secrets[owner] = append(r.secrets[owner], value)
	r.version++
	clear(r.cache)
}

// Forget drops owner's secrets.
func (r *Registry) Forget(owner string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.secrets[owner]; !ok {
		return
	}
	delete(r.secrets, owner)
	r.version++
	clear(r.cache)
}

// Count is how many secrets owner has.
func (r *Registry) Count(owner string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.secrets[owner])
}

// For is the Scrubber for the union of owners' secrets as they are when it
// is called, nil when they have none. A Scrubber that isn't cached is
// built outside the lock and cached only if no secret changed meanwhile.
func (r *Registry) For(owners ...string) *Scrubber {
	r.mu.Lock()
	var with []string
	for _, o := range owners {
		if len(r.secrets[o]) > 0 {
			with = append(with, o)
		}
	}
	if len(with) == 0 {
		r.mu.Unlock()
		return nil
	}
	slices.Sort(with)
	with = slices.Compact(with)
	key := strings.Join(with, "\x00")
	if s, ok := r.cache[key]; ok {
		r.mu.Unlock()
		return s
	}
	var all []string
	for _, o := range with {
		all = append(all, r.secrets[o]...)
	}
	version := r.version
	r.mu.Unlock()

	s := build(all)

	r.mu.Lock()
	defer r.mu.Unlock()
	if r.version == version {
		if len(r.cache) >= maxCached {
			clear(r.cache)
		}
		r.cache[key] = s
	}
	return s
}
