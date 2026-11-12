package pagetools

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
)

// maxRefs caps the refs a session keeps live; past it, the oldest
// document's go first.
const maxRefs = 5000

// A ref names one element of one document for the agent: "a7e12" is
// element 12 of the session's ref map whose epoch is "a7". The epoch is
// random per map, so after an orchestrator restart (browsers survive it)
// old refs answer stale instead of naming new elements. Refs are never
// reused: one names one node of one document, until it goes stale.
var refPattern = regexp.MustCompile(`^([a-z][a-z0-9])e([1-9][0-9]{0,8})$`)

// refKey is what a ref stands for.
type refKey struct {
	target  string // the tab
	frame   string
	loader  string // the frame's document
	backend int64  // the node, by CDP backend node ID
}

type docKey struct{ target, loader string }

// refs is one session's ref map.
type refs struct {
	epoch string
	next  int
	byRef map[string]refKey
	byKey map[refKey]string
	// docs lists documents by first use, for dropping the oldest first;
	// perDoc counts each one's live refs.
	docs   []docKey
	perDoc map[docKey][]string
}

func newRefs() *refs {
	const letters = "abcdefghijklmnopqrstuvwxyz"
	const alnum = letters + "0123456789"
	pick := func(set string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[n.Int64()]
	}
	return &refs{epoch: string([]byte{pick(letters), pick(alnum)}),
		byRef: map[string]refKey{}, byKey: map[refKey]string{}, perDoc: map[docKey][]string{}}
}

// ref returns the node's ref, making one if it has none.
func (r *refs) ref(k refKey) string {
	if ref, ok := r.byKey[k]; ok {
		return ref
	}
	r.next++
	ref := r.epoch + "e" + strconv.Itoa(r.next)
	r.byRef[ref] = k
	r.byKey[k] = ref
	d := docKey{k.target, k.loader}
	if _, ok := r.perDoc[d]; !ok {
		r.docs = append(r.docs, d)
	}
	r.perDoc[d] = append(r.perDoc[d], ref)
	for len(r.byRef) > maxRefs {
		r.evict()
	}
	return ref
}

// evict drops the oldest document's refs; if that document is the only
// one, its oldest refs.
func (r *refs) evict() {
	if len(r.docs) == 0 {
		return
	}
	d := r.docs[0]
	list := r.perDoc[d]
	if len(r.docs) > 1 {
		for _, ref := range list {
			r.forget(ref)
		}
		r.docs = r.docs[1:]
		delete(r.perDoc, d)
		return
	}
	drop := len(r.byRef) - maxRefs
	for _, ref := range list[:drop] {
		r.forget(ref)
	}
	r.perDoc[d] = list[drop:]
}

func (r *refs) forget(ref string) {
	if k, ok := r.byRef[ref]; ok {
		delete(r.byKey, k)
		delete(r.byRef, ref)
	}
}

// dropAll forgets every ref; the counter goes on, so none is reused.
func (r *refs) dropAll() {
	clear(r.byRef)
	clear(r.byKey)
	clear(r.perDoc)
	r.docs = nil
}

// dropTarget forgets a closed tab's refs.
func (r *refs) dropTarget(target string) {
	kept := r.docs[:0]
	for _, d := range r.docs {
		if d.target != target {
			kept = append(kept, d)
			continue
		}
		for _, ref := range r.perDoc[d] {
			r.forget(ref)
		}
		delete(r.perDoc, d)
	}
	r.docs = kept
}

// dropStale forgets a tab's refs to documents other than loader.
func (r *refs) dropStale(target, loader string) {
	kept := r.docs[:0]
	for _, d := range r.docs {
		if d.target != target || d.loader == loader {
			kept = append(kept, d)
			continue
		}
		for _, ref := range r.perDoc[d] {
			r.forget(ref)
		}
		delete(r.perDoc, d)
	}
	r.docs = kept
}

// errStale is the answer for a ref that no longer names anything.
func errStale(ref string) error {
	return &Error{Status: 409, Msg: fmt.Sprintf("stale ref %s: take a new snapshot", ref)}
}

// lookup finds what a ref stands for. A malformed ref is a bad request;
// one from another map (another session, or before a restart) or one
// dropped since is stale.
func (r *refs) lookup(ref string) (refKey, error) {
	m := refPattern.FindStringSubmatch(ref)
	if m == nil {
		return refKey{}, &Error{Status: 400, Msg: fmt.Sprintf("%q isn't a ref; refs look like a7e12 and come from snapshot or find", ref)}
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
