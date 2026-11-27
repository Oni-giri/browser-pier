package visualtree

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"net/http"
	"regexp"
	"strconv"
)

// maxRefs caps the refs a session keeps live; past it, the oldest go.
const maxRefs = 5000

// A Visual-mode ref names one element of one document: "vk3n12" is
// element 12 of the session's ref map whose epoch is "k3". The epoch is
// random per map, so after an orchestrator restart old refs answer stale
// instead of naming new elements, and numbers are never reused. The "v"
// and "n" keep them apart from the page tools' refs ("a7e12"): neither
// pattern matches the other's.
var refPattern = regexp.MustCompile(`^v([a-z][a-z0-9])n([1-9][0-9]{0,8})$`)

// pageToolsRef is the page tools' ref (internal/pagetools).
var pageToolsRef = regexp.MustCompile(`^[a-z][a-z0-9]e[1-9][0-9]{0,8}$`)

// IsRef reports whether s looks like a Visual-mode ref.
func IsRef(s string) bool { return refPattern.MatchString(s) }

// refKey is what a ref stands for: an object on the accessibility bus, in
// a document. A new document (a navigation) has a new object, so a ref
// from before it never names something on the new page.
type refKey struct{ app, doc, id string }

type refs struct {
	epoch string
	next  int
	byRef map[string]refKey
	byKey map[refKey]string
	order []string // by when made, for dropping the oldest
}

func newRefs() *refs {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const alnum = letters + "0123456789"
	pick := func(set string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[n.Int64()]
	}
	return &refs{epoch: string([]byte{pick(letters), pick(alnum)}), byRef: map[string]refKey{}, byKey: map[refKey]string{}}
}

// ref returns the element's ref, making one if it has none.
func (r *refs) ref(k refKey) string {
	if ref, ok := r.byKey[k]; ok {
		return ref
	}
	r.next++
	ref := "v" + r.epoch + "n" + strconv.Itoa(r.next)
	r.byRef[ref] = k
	r.byKey[k] = ref
	r.order = append(r.order, ref)
	for len(r.byRef) > maxRefs {
		r.forget(r.order[0])
		r.order = r.order[1:]
	}
	return ref
}

func (r *refs) forget(ref string) {
	if k, ok := r.byRef[ref]; ok {
		delete(r.byKey, k)
		delete(r.byRef, ref)
	}
}

// keepDoc forgets the refs of every other document: once a snapshot reads
// a new one, the old page's refs are stale.
func (r *refs) keepDoc(app, doc string) {
	kept := r.order[:0]
	for _, ref := range r.order {
		k, ok := r.byRef[ref]
		if !ok {
			continue
		}
		if k.app != app || k.doc != doc {
			r.forget(ref)
			continue
		}
		kept = append(kept, ref)
	}
	r.order = kept
}

// dropAll forgets every ref; the counter goes on, so none is reused.
func (r *refs) dropAll() {
	clear(r.byRef)
	clear(r.byKey)
	r.order = nil
}

func errStale(ref string) error {
	return &Error{Status: http.StatusConflict, Msg: fmt.Sprintf("stale ref %s: take a new visual_snapshot", ref)}
}

// lookup finds what a ref stands for. A malformed ref, or one of the page
// tools', is a bad request; one from another map (another session, or
// before a restart) or one dropped since is stale.
func (r *refs) lookup(ref string) (refKey, error) {
	m := refPattern.FindStringSubmatch(ref)
	if m == nil {
		if pageToolsRef.MatchString(ref) {
			return refKey{}, &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf("%s is a ref of the page tools (snapshot, find), which are for API mode; in Visual mode, click takes refs from visual_snapshot, like vk3n12", ref)}
		}
		return refKey{}, &Error{Status: http.StatusBadRequest, Msg: fmt.Sprintf("%q isn't a ref; Visual-mode refs look like vk3n12 and come from visual_snapshot", ref)}
	}
	if m[1] != r.epoch {
		return refKey{}, errStale(ref)
	}
	k, ok := r.byRef[ref]
	if !ok {
		return refKey{}, errStale(ref)
	}
	return k, nil
}
