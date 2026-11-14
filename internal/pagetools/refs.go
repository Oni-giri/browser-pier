package pagetools

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"regexp"
	"slices"
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

// ref returns the node's ref, making one if it has none. Its document
// becomes the most recently used, the last to be dropped.
func (r *refs) ref(k refKey) string {
	d := docKey{k.target, k.loader}
	r.touch(d)
	if ref, ok := r.byKey[k]; ok {
		return ref
	}
	r.next++
	ref := r.epoch + "e" + strconv.Itoa(r.next)
	r.byRef[ref] = k
	r.byKey[k] = ref
	r.perDoc[d] = append(r.perDoc[d], ref)
	for len(r.byRef) > maxRefs {
		r.evict(d)
	}
	return ref
}

// touch makes a document the most recently used.
func (r *refs) touch(d docKey) {
	if i := slices.Index(r.docs, d); i >= 0 {
		if i == len(r.docs)-1 {
			return
		}
		r.docs = slices.Delete(r.docs, i, i+1)
	}
	r.docs = append(r.docs, d)
}

// evict drops the least recently used document's refs, never those of
// cur, the document being read. If cur is the only one, its oldest refs
// go (refs from earlier calls: one call hands out far fewer than maxRefs).
func (r *refs) evict(cur docKey) {
	for i, d := range r.docs {
		if d == cur {
			continue
		}
		for _, ref := range r.perDoc[d] {
			r.forget(ref)
		}
		r.docs = slices.Delete(r.docs, i, i+1)
		delete(r.perDoc, d)
		return
	}
	list := r.perDoc[cur]
	drop := min(len(r.byRef)-maxRefs, len(list)-1)
	if drop <= 0 {
		return
	}
	for _, ref := range list[:drop] {
		r.forget(ref)
	}
	r.perDoc[cur] = list[drop:]
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
