package chromium

import (
	"bytes"
	"errors"
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

// The loader's gates, against a stand-in for the kernel's AppArmor files.
// It must load only where needed, and fail without writing anything when
// it isn't root on the host (rootless Docker or Podman).
func TestLoadGates(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("as root this would try to load the profile")
	}
	// How a kernel says it mediates user namespaces: Ubuntu's list it as a
	// file, Debian's 6.12 as a word in namespaces/mask (no such file).
	for _, kernel := range []struct {
		name     string
		mediates func(namespaces string) error
	}{
		{"Ubuntu", func(ns string) error { return os.WriteFile(filepath.Join(ns, "userns_create"), nil, 0o644) }},
		{"Debian", func(ns string) error {
			return os.WriteFile(filepath.Join(ns, "mask"), []byte("userns_create\n"), 0o644)
		}},
	} {
		t.Run(kernel.name, func(t *testing.T) {
			dir := t.TempDir()
			aa := filepath.Join(dir, "apparmor")
			ns := filepath.Join(aa, "features", "namespaces")
			restrict := filepath.Join(dir, "restrict")
			etc := filepath.Join(dir, "etc")
			for _, d := range []string{ns, filepath.Join(etc, "abi")} {
				if err := os.MkdirAll(d, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			touch := func(path string) {
				t.Helper()
				if err := os.WriteFile(path, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			run := func(fs string) (int, string) {
				t.Helper()
				cmd := exec.Command(filepath.Join("apparmor", "load"), "--persist", etc)
				cmd.Env = append(os.Environ(), "BROWSERPIER_APPARMOR_FS="+fs, "BROWSERPIER_APPARMOR_RESTRICT="+restrict)
				out, err := cmd.CombinedOutput()
				code := 0
				if exit, ok := err.(*exec.ExitError); ok { //nolint:errorlint // only the exit status
					code = exit.ExitCode()
				} else if err != nil {
					t.Fatal(err)
				}
				return code, string(out)
			}
			expect := func(what string, wantCode int, want string, fs string) {
				t.Helper()
				if code, out := run(fs); code != wantCode || !strings.Contains(out, want) {
					t.Errorf("%s: exit %d, %q; want exit %d with %q", what, code, out, wantCode, want)
				}
			}

			expect("no AppArmor", 0, "nothing to load", filepath.Join(dir, "none"))
			expect("no userns mediation (Debian 12)", 0, "doesn't mediate user namespaces", aa)
			// A mask without userns_create mediates other things only.
			if err := os.WriteFile(filepath.Join(ns, "mask"), []byte("pivot_root\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			expect("a mask without userns_create", 0, "doesn't mediate user namespaces", aa)
			if err := kernel.mediates(ns); err != nil {
				t.Fatal(err)
			}
			touch(restrict)
			expect("Ubuntu's restriction", 0, "Ubuntu's user namespace restriction", aa)
			if err := os.Remove(restrict); err != nil {
				t.Fatal(err)
			}
			expect("AppArmor 3 userspace, newer kernel", 0, "older than 4.0", aa)
			touch(filepath.Join(etc, "abi", "4.0"))
			// Past every gate: it would load here, but as no root it can't.
			expect("not root (rootless)", 1, "isn't root on the host", aa)
			if _, err := os.Stat(filepath.Join(etc, "browserpier")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("the profile was installed without being loaded: %v", err)
			}
		})
	}
}

// Compiles the profile without loading it, where AppArmor 4's parser is
// here: no root needed. A normal user's PATH may lack the sbin directories
// it lives in (Debian).
func TestAppArmorProfileCompiles(t *testing.T) {
	parser, err := exec.LookPath("apparmor_parser")
	for _, p := range []string{"/usr/sbin/apparmor_parser", "/sbin/apparmor_parser"} {
		if err == nil {
			break
		}
		if _, statErr := os.Stat(p); statErr == nil {
			parser, err = p, nil
		}
	}
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
