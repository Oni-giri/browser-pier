//go:build docker

package docker

import (
	"context"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/client"
)

// The sandbox check runs the real browser image under the browser
// containers' settings. BROWSERPIER_TEST_IMAGE names the image (see
// `make test-docker`), and BROWSERPIER_USERNS_MODE is passed through.
func TestCheckSandbox(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set; run `make test-docker`")
	}
	c, err := New(Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), User: User(os.Getuid(), os.Getgid()),
		Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	dir := t.TempDir()
	if err := c.UseDataDir(ctx, dir, dir); err != nil {
		t.Fatal(err)
	}

	// The image carries the check, and says so.
	c.plan = appArmorPlan{try: []string{""}}
	if ok, why := c.checkable(ctx, false); !ok {
		t.Fatalf("the browser image isn't checkable: %q", why)
	}
	// Wherever the browser tests pass, the sandbox works under the
	// daemon's default profile, as the browsers' user.
	if v, why := c.checkSandbox(ctx, ""); v != works {
		t.Fatalf("the sandbox check fails under the daemon's default: %v %s", v, why)
	}
	name := "browserpier_check_" + c.instance
	if _, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); !cerrdefs.IsNotFound(err) {
		t.Fatalf("the check container was left behind: %v", err)
	}

	// A profile that isn't loaded fails the check, rather than being
	// unknown, on a daemon that applies AppArmor.
	info, err := c.api.Info(ctx, client.InfoOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(info.Info.SecurityOptions, "name=apparmor") && !slices.Contains(info.Info.SecurityOptions, "name=apparmor,profile=default") ||
		slices.Contains(info.Info.SecurityOptions, "name=rootless") {
		t.Skip("this daemon applies no AppArmor profile")
	}
	v, why := c.checkSandbox(ctx, "apparmor=browserpier-test-not-loaded")
	if v != fails || why == "" {
		t.Fatalf("a profile that isn't loaded: %v, why %q", v, why)
	}
	t.Logf("not loaded: %s", why)
}
