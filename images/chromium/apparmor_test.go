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

var umbrelHook = filepath.Join("..", "..", "umbrel", "yakitori-browserpier", "hooks", "pre-start")

// heredoc returns the body of the heredoc that script opens with <<'delim'.
func heredoc(t *testing.T, script []byte, delim string) []byte {
	t.Helper()
	_, rest, ok := bytes.Cut(script, []byte("<<'"+delim+"'"))
	if !ok {
		t.Fatalf("no <<'%s'", delim)
	}
	if bytes.Contains(rest, []byte("<<'"+delim+"'")) {
		t.Fatalf("<<'%s' opened twice", delim)
	}
	_, body, ok := bytes.Cut(rest, []byte("\n"))
	if !ok {
		t.Fatalf("<<'%s' has no body", delim)
	}
	if bytes.HasPrefix(body, []byte(delim+"\n")) {
		return nil
	}
	body, _, ok = bytes.Cut(body, []byte("\n"+delim+"\n"))
	if !ok {
		t.Fatalf("<<'%s' is never closed", delim)
	}
	return append(body, '\n')
}

// Umbrel's app update copies only the files it knows, so the pre-start
// hook carries the profile and the loader itself, and they mustn't drift.
func TestUmbrelHookCarriesTheAppArmorFiles(t *testing.T) {
	hook := readFile(t, umbrelHook)
	for _, h := range []struct{ delim, file string }{
		{"BROWSERPIER_PROFILE", "browserpier"},
		{"BROWSERPIER_LOADER", "load"},
	} {
		ours := filepath.Join("apparmor", h.file)
		if !bytes.Equal(heredoc(t, hook, h.delim), readFile(t, ours)) {
			t.Errorf("%s's copy of %s differs: go generate -run gen_umbrel_hook ./images/chromium", umbrelHook, ours)
		}
	}
	// Nothing the update would leave behind.
	if _, err := os.Stat(filepath.Join(filepath.Dir(umbrelHook), "..", "apparmor")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the Umbrel app has an apparmor folder again, which Umbrel's update doesn't copy: %v", err)
	}
	for _, f := range []string{filepath.Join("apparmor", "load"), umbrelHook} {
		info, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode()&0o111 == 0 {
			t.Errorf("%s isn't executable", f)
		}
	}
}

// The hook, alone in a folder as after an Umbrel update, past every gate
// of a stand-in Debian 13 kernel, with stand-ins for id and the parser:
// it hands the parser the profile, says what happened in one line, exits
// 0 whatever happens, and leaves no temporary files.
func TestUmbrelHook(t *testing.T) {
	if _, err := os.Stat("/etc/apparmor.d/abi/4.0"); err != nil {
		t.Skip("the loader reads the host's /etc/apparmor.d/abi/4.0, not here")
	}
	if uidMap, err := os.ReadFile("/proc/self/uid_map"); err != nil || len(strings.Fields(string(uidMap))) != 3 || strings.Fields(string(uidMap))[2] != "4294967295" {
		t.Skip("not in the host's user namespace")
	}
	dir := t.TempDir()
	app := filepath.Join(dir, "app", "hooks")
	bin := filepath.Join(dir, "bin")
	tmp := filepath.Join(dir, "tmp")
	ns := filepath.Join(dir, "apparmor", "features", "namespaces")
	for _, d := range []string{app, bin, tmp, ns} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string, mode os.FileMode) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	hook := filepath.Join(app, "pre-start")
	write(hook, string(readFile(t, umbrelHook)), 0o755)
	write(filepath.Join(ns, "mask"), "userns_create\n", 0o644)
	write(filepath.Join(bin, "id"), "#!/bin/sh\necho 0\n", 0o755)
	got := filepath.Join(dir, "parsed")
	// $PARSER_FAILS makes it fail, as a parser refusing the profile does.
	write(filepath.Join(bin, "apparmor_parser"), `#!/bin/sh
for a; do last="$a"; done
echo "$*" >"`+got+`.args"
cp "$last" "`+got+`"
if [ -n "${PARSER_FAILS:-}" ]; then echo "AppArmor parser error: line 1"; echo "second line" >&2; exit 1; fi
`, 0o755)

	run := func(env ...string) (string, string) {
		t.Helper()
		cmd := exec.Command(hook)
		cmd.Dir = dir
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "TMPDIR="+tmp,
			"BROWSERPIER_APPARMOR_FS="+filepath.Join(dir, "apparmor"), "BROWSERPIER_APPARMOR_RESTRICT="+filepath.Join(dir, "none"))
		cmd.Env = append(cmd.Env, env...)
		if err := cmd.Run(); err != nil {
			t.Fatalf("the hook must exit 0: %v\n%s%s", err, stdout.String(), stderr.String())
		}
		if left, _ := os.ReadDir(tmp); len(left) > 0 {
			t.Errorf("the hook left %v in TMPDIR", left)
		}
		return stdout.String(), stderr.String()
	}
	oneLine := func(what, out, want string) {
		t.Helper()
		if strings.Count(out, "\n") != 1 || !strings.HasPrefix(out, "BrowserPier AppArmor: "+want) {
			t.Errorf("%s: %q; want one line starting %q", what, out, want)
		}
	}

	// Every outcome goes to stderr: umbreld's journal is known to keep a
	// hook's stderr (0.2.0's missing-loader error showed up there).
	stdout, stderr := run()
	oneLine("loaded", stderr, "loaded the browserpier profile")
	if stdout != "" {
		t.Errorf("loaded: stdout %q", stdout)
	}
	if !bytes.Equal(readFile(t, got), readFile(t, filepath.Join("apparmor", "browserpier"))) {
		t.Error("the parser didn't get the profile")
	}
	if args := string(readFile(t, got+".args")); !strings.HasPrefix(args, "--replace --skip-cache ") {
		t.Errorf("parser args %q", args)
	}

	stdout, stderr = run("PARSER_FAILS=1")
	oneLine("parser fails", stderr, "couldn't load the browserpier profile: "+filepath.Join(bin, "apparmor_parser")+" failed: AppArmor parser error: line 1 second line")
	if stdout != "" {
		t.Errorf("parser fails: stdout %q", stdout)
	}

	stdout, stderr = run("BROWSERPIER_APPARMOR_FS=" + filepath.Join(dir, "none"))
	oneLine("no AppArmor", stderr, "browserpier profile not needed here: AppArmor isn't on")
	if stdout != "" {
		t.Errorf("no AppArmor: stdout %q", stdout)
	}

	// No usable temporary directory: it still says so and exits 0.
	_, stderr = run("TMPDIR=" + filepath.Join(dir, "missing"))
	oneLine("no TMPDIR", stderr, "couldn't load the browserpier profile: no temporary directory")
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

			expect("no AppArmor", 0, "browserpier profile not needed here: AppArmor isn't on", filepath.Join(dir, "none"))
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
			expect("not root (rootless)", 1, "couldn't load the browserpier profile: this isn't root on the host", aa)
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
