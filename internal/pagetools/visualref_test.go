package pagetools

import (
	"errors"
	"strings"
	"testing"
)

// TestVisualRefsAreRefused (PROJ-150): a Visual-mode ref from
// visual_snapshot ("vk3n12") is no page tools ref, whatever its epoch.
func TestVisualRefsAreRefused(t *testing.T) {
	r := newRefs()
	for _, ref := range []string{"vk3n12", "vaen1", "v" + r.epoch + "n1"} {
		_, err := r.lookup(ref)
		var e *Error
		if !errors.As(err, &e) || e.Status != 400 || !strings.Contains(e.Msg, "isn't a ref") {
			t.Errorf("%s: %v", ref, err)
		}
	}
}
