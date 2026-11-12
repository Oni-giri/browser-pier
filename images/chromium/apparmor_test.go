package chromium

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The profile and its loader, as the image and the Umbrel app ship them.
var apparmorFiles = []string{"browserpier", "load"}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path) //nolint:gosec // files in this repository
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestAppArmorProfile(t *testing.T) {
	p := string(readFile(t, filepath.Join("apparmor", "browserpier")))
	// AppArmor 4's ABI, literally and first: it's the one that mediates
	// user namespaces, and the last where "network," covers unix sockets.
	if !strings.HasPrefix(p, "abi <abi/4.0>,\n") {
		t.Error("the profile must start with abi <abi/4.0>,")
	}
	for _, rule := range []string{
		"profile browserpier flags=(attach_disconnected,mediate_deleted) {",
		"\n  userns,\n",
		"\n  deny mount,\n",
		"\n  deny /sys/kernel/security/** rwklx,\n",
		"signal (receive) peer=runc,", // docker stop, so Chromium closes gracefully
		"signal (send,receive) peer=browserpier,",
		"ptrace (trace,tracedby,read,readby) peer=browserpier,",
	} {
		if !strings.Contains(p, rule) {
			t.Errorf("the profile lacks %q", rule)
		}
	}
}

// The Umbrel app carries copies; Umbrel's hook can't reach the image.
func TestUmbrelCarriesTheAppArmorFiles(t *testing.T) {
	for _, f := range apparmorFiles {
		ours := filepath.Join("apparmor", f)
		umbrel := filepath.Join("..", "..", "umbrel", "yakitori-browserpier", "apparmor", f)
		if !bytes.Equal(readFile(t, ours), readFile(t, umbrel)) {
			t.Errorf("%s differs from %s: cp %s %s", umbrel, ours, ours, umbrel)
		}
	}
	for _, f := range []string{
		filepath.Join("apparmor", "load"),
		filepath.Join("..", "..", "umbrel", "yakitori-browserpier", "apparmor", "load"),
		filepath.Join("..", "..", "umbrel", "yakitori-browserpier", "hooks", "pre-start"),
	} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s isn't executable", f)
		}
	}
}

// Compiles the profile without loading it, where AppArmor 4's parser is
// here: no root needed.
func TestAppArmorProfileCompiles(t *testing.T) {
	parser, err := exec.LookPath("apparmor_parser")
	if err != nil {
		t.Skip("no apparmor_parser here")
	}
	if _, err := os.Stat("/etc/apparmor.d/abi/4.0"); err != nil {
		t.Skip("AppArmor here is older than 4.0")
	}
	out, err := exec.Command(parser, "--skip-kernel-load", "--skip-cache", "--stdout", filepath.Join("apparmor", "browserpier")).CombinedOutput() //nolint:gosec // our own profile
	if err != nil {
		t.Fatalf("apparmor_parser: %v\n%s", err, out)
	}
}
