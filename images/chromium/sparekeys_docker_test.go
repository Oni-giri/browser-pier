//go:build docker

package chromium

import (
	"os/exec"
	"regexp"
	"strconv"
	"testing"

	"github.com/Oni-giri/browser-pier/internal/keyplan"
)

// TestImageHasEnoughSpareKeys checks the image's keyboard map leaves at
// least keyplan.SpareKeys keys for typing to bind characters the map
// lacks to (PROJ-195): the orchestrator's time estimates (MCP type_text's
// 45 s refusal, the live view's Type button's wait) assume as many. The
// image's build runs the same check.
func TestImageHasEnoughSpareKeys(t *testing.T) {
	image := testImage(t)
	out, err := exec.Command("docker", "run", "--rm", image, "spare-keys").CombinedOutput() //nolint:gosec // test arguments
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("spare-keys: %v", err)
	}
	m := regexp.MustCompile(`spare keys: (\d+)`).FindSubmatch(out)
	if m == nil {
		t.Fatal("no count")
	}
	if n, _ := strconv.Atoi(string(m[1])); n < keyplan.SpareKeys {
		t.Fatalf("%d spare keys, want at least %d", n, keyplan.SpareKeys)
	}
}
