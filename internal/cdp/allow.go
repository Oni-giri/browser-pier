package cdp

import (
	"fmt"
	"slices"
	"strings"
)

// AllowList is the set of CDP methods one caller may send. Each caller
// (the page tools, the content endpoints, a checker) builds its own with
// NewAllowList and keeps it unexported, so no other caller can borrow it:
// a list that allows input or DOM writes is never handed to code that
// shouldn't have them.
//
// Some methods are never allowed, whoever asks:
//   - any domain's enable (Runtime.enable, Page.enable, DOM.enable…): an
//     enabled domain is what pages detect, and it streams events that
//     would keep the connection busy;
//   - DOM.getDocument and DOM.requestChildNodes, which enable the DOM
//     agent's node tracking implicitly;
//   - code anywhere but an isolated world the connection created and
//     verified (see Conn.check): Runtime.evaluate only reads a world's
//     globalThis, and Runtime.callFunctionOn only runs on objects from
//     such a world, so code never runs in the page's own world.
type AllowList struct {
	name    string
	methods map[string]bool
}

// banned are methods no allow-list may name.
var banned = []string{"DOM.getDocument", "DOM.getFlattenedDocument", "DOM.requestChildNodes", "Runtime.runIfWaitingForDebugger", "Target.setAutoAttach", "Target.setDiscoverTargets", "Target.exposeDevToolsProtocol"}

// NewAllowList makes a caller's allow-list. name says who it is for, in
// errors. It panics on a method no caller may have (an enable, or one of
// the banned methods above): that is a programming error.
func NewAllowList(name string, methods ...string) *AllowList {
	a := &AllowList{name: name, methods: map[string]bool{}}
	for _, m := range methods {
		if err := forbidden(m); err != nil {
			panic(fmt.Sprintf("cdp: allow-list %s: %v", name, err))
		}
		a.methods[m] = true
	}
	return a
}

func forbidden(method string) error {
	domain, verb, ok := strings.Cut(method, ".")
	if !ok || domain == "" || verb == "" {
		return fmt.Errorf("%q isn't a CDP method", method)
	}
	if verb == "enable" || slices.Contains(banned, method) {
		return fmt.Errorf("%s is never allowed", method)
	}
	return nil
}

// Allows reports whether the list names method.
func (a *AllowList) Allows(method string) bool { return a != nil && a.methods[method] }

// Methods lists the allowed methods, sorted, for tests and docs.
func (a *AllowList) Methods() []string {
	out := make([]string, 0, len(a.methods))
	for m := range a.methods {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// Name is who the list is for.
func (a *AllowList) Name() string { return a.name }
