package docker

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
)

func TestBrowserConfigLocksDownTheContainer(t *testing.T) {
	spec := BrowserSpec{
		Name: "browserpier-google", Profile: "google", Session: "s1",
		ProfileDir: "/srv/bp/profiles/google/chrome", ControlDir: "/srv/bp/profiles/google/run", FilesDir: "/srv/bp/profiles/google/files",
		User: "1000:1000", Screen: "1440x900", MountCheck: "s1",
	}
	cfg, host := browserConfig("ghcr.io/oni-giri/browser-pier-chromium:1.0.0", `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`, "", spec)

	if cfg.Image != "ghcr.io/oni-giri/browser-pier-chromium:1.0.0" || cfg.User != "1000:1000" {
		t.Fatalf("image %q, user %q", cfg.Image, cfg.User)
	}
	if len(cfg.ExposedPorts) != 0 || len(host.PortBindings) != 0 || host.PublishAllPorts {
		t.Fatal("a browser container must publish no ports")
	}
	if host.NetworkMode.IsHost() || host.Privileged || host.PidMode.IsHost() || host.IpcMode.IsHost() {
		t.Fatal("the browser container must not share host namespaces or run privileged")
	}
	if len(host.CapAdd) != 1 || host.CapAdd[0] != "SYS_CHROOT" || len(host.CapDrop) != 1 || host.CapDrop[0] != "ALL" {
		t.Fatalf("caps add %v drop %v; want ALL dropped and only SYS_CHROOT (for the sandbox) back", host.CapAdd, host.CapDrop)
	}
	opts := strings.Join(host.SecurityOpt, " ")
	if !strings.Contains(opts, `seccomp={"defaultAction":"SCMP_ACT_ERRNO"}`) || !strings.Contains(opts, "no-new-privileges:true") {
		t.Fatalf("security opts %v", host.SecurityOpt)
	}
	if strings.Contains(opts, "unconfined") {
		t.Fatalf("security opts %v loosen a default", host.SecurityOpt)
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout < 30 {
		t.Fatalf("stop timeout %v; the entrypoint needs at least 30 s", cfg.StopTimeout)
	}
	if host.ShmSize < 512<<20 {
		t.Fatalf("shm %d", host.ShmSize)
	}
	if host.Init == nil || *host.Init {
		t.Fatal("the image's entrypoint must be PID 1")
	}

	wantMounts := map[string]string{"/profile": spec.ProfileDir, "/run/browserpier": spec.ControlDir, "/files": spec.FilesDir}
	if len(host.Mounts) != len(wantMounts) || len(host.Binds) != 0 {
		t.Fatalf("mounts %+v binds %v", host.Mounts, host.Binds)
	}
	for _, m := range host.Mounts {
		if m.Type != mount.TypeBind || wantMounts[m.Target] != m.Source {
			t.Fatalf("mount %+v", m)
		}
	}

	env := strings.Join(cfg.Env, "\n")
	for _, want := range []string{"BROWSERPIER_SCREEN=1440x900", "BROWSERPIER_MOUNT_CHECK=s1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("env %v lacks %s", cfg.Env, want)
		}
	}
	if cfg.Labels[LabelManaged] != "true" || cfg.Labels[LabelProfile] != "google" || cfg.Labels[LabelSession] != "s1" || cfg.Labels[LabelSpec] != Spec {
		t.Fatalf("labels %v", cfg.Labels)
	}
}

func TestNewCompactsTheEmbeddedSeccompProfile(t *testing.T) {
	c, err := New(Options{Image: "img", Instance: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if c.seccompOpt != "seccomp="+c.seccomp {
		t.Fatalf("Docker takes the profile inline; got %.40s…", c.seccompOpt)
	}
	if strings.ContainsAny(c.seccomp, "\n\t") || !json.Valid([]byte(c.seccomp)) || !strings.Contains(c.seccomp, `"unshare"`) {
		t.Fatalf("seccomp profile not compact valid JSON with the sandbox rules: %.80s…", c.seccomp)
	}
	if _, err := New(Options{Instance: "i1"}); err == nil {
		t.Fatal("New must refuse an empty image")
	}
	if _, err := New(Options{Image: "img"}); err == nil {
		t.Fatal("New must refuse an empty instance ID")
	}
}

func TestHostPath(t *testing.T) {
	mounts := []container.MountPoint{
		{Type: mount.TypeBind, Source: "/home/u/bp", Destination: "/data"},
		{Type: mount.TypeVolume, Source: "/var/lib/docker/volumes/x/_data", Destination: "/data/profiles"},
		{Type: mount.TypeTmpfs, Destination: "/tmp"},
		{Type: mount.TypeBind, Source: "/srv", Destination: "/dat"},
	}
	cases := []struct {
		path, want string
		ok         bool
	}{
		{"/data", "/home/u/bp", true},
		{"/data/db", "/home/u/bp/db", true},
		{"/data/profiles/google", "/var/lib/docker/volumes/x/_data/google", true}, // the deepest mount wins
		{"/database", "", false},                                                  // not under /data despite the prefix
		{"/tmp/x", "", false},                                                     // tmpfs has no host path
		{"/elsewhere", "", false},
	}
	for _, c := range cases {
		got, ok := hostPath(mounts, c.path)
		if got != c.want || ok != c.ok {
			t.Errorf("hostPath(%q) = %q, %t; want %q, %t", c.path, got, ok, c.want, c.ok)
		}
	}
}

func TestContainerIDFromMountinfo(t *testing.T) {
	id := strings.Repeat("ab12", 16)
	docker := "542 520 0:52 / / rw,relatime master:250 - overlay overlay rw\n" +
		"560 542 8:1 /var/lib/docker/containers/" + id + "/resolv.conf /etc/resolv.conf rw,relatime - ext4 /dev/sda1 rw\n" +
		"561 542 8:1 /var/lib/docker/containers/" + id + "/hostname /etc/hostname rw,relatime - ext4 /dev/sda1 rw\n"
	if got := containerIDFromMountinfo(strings.NewReader(docker)); got != id {
		t.Fatalf("docker: got %q", got)
	}
	podman := "700 680 0:40 /containers/storage/overlay-containers/" + id + "/userdata/hostname /etc/hostname rw - tmpfs tmpfs rw\n"
	if got := containerIDFromMountinfo(strings.NewReader(podman)); got != id {
		t.Fatalf("podman: got %q", got)
	}
	host := "22 1 8:1 / / rw,relatime shared:1 - ext4 /dev/sda1 rw\n"
	if got := containerIDFromMountinfo(strings.NewReader(host)); got != "" {
		t.Fatalf("host: got %q", got)
	}
}

func TestLastLine(t *testing.T) {
	if got := lastLine([]byte("one\ntwo\n\n  \n")); got != "two" {
		t.Fatalf("got %q", got)
	}
	if got := lastLine(nil); got != "" {
		t.Fatalf("got %q", got)
	}
	long := strings.Repeat("é", maxLogLine) // twice the cap, in bytes
	if got := lastLine([]byte(long)); len(got) > maxLogLine+len("…") || !strings.HasSuffix(got, "…") {
		t.Fatalf("long line kept %d bytes", len(got))
	}
}

func TestAppArmorOpt(t *testing.T) {
	aa := []string{"name=apparmor", "name=seccomp,profile=builtin", "name=cgroupns"}
	for _, c := range []struct {
		daemon      []string
		profile     string
		restriction string
		want        string
	}{
		// Debian 13 and umbrelOS: docker-default would refuse the sandbox.
		{aa, "", "absent", "apparmor=unconfined"},
		{[]string{"name=apparmor,profile=default", "name=seccomp,profile=builtin"}, "", "absent", "apparmor=unconfined"},
		// A default profile the admin set for the daemon may allow userns.
		{[]string{"name=seccomp,profile=builtin", "name=apparmor,profile=/etc/docker/apparmor.profile"}, "", "absent", ""},
		// Ubuntu: unconfined would lose the sandbox, docker-default works.
		{aa, "", "present", ""},
		// No AppArmor on the daemon: leave the option out.
		{[]string{"name=seccomp,profile=builtin", "name=selinux"}, "", "absent", ""},
		{nil, "", "absent", ""},
		// A profile named in BROWSERPIER_BROWSER_APPARMOR wins.
		{aa, "browserpier", "absent", "apparmor=browserpier"},
		{aa, "unconfined", "present", "apparmor=unconfined"},
		{aa, "docker-default", "absent", "apparmor=docker-default"},
	} {
		if got := appArmorOpt(c.daemon, c.profile, c.restriction); got != c.want {
			t.Errorf("appArmorOpt(%v, %q, %q) = %q, want %q", c.daemon, c.profile, c.restriction, got, c.want)
		}
	}
}

func TestBrowserConfigAppArmor(t *testing.T) {
	spec := BrowserSpec{Name: "browserpier-google", Profile: "google", Session: "s1", User: "1000:1000", Screen: "1440x900"}
	_, host := browserConfig("img", "seccomp={}", "apparmor=unconfined", spec)
	if len(host.SecurityOpt) != 3 || host.SecurityOpt[0] != "seccomp={}" || host.SecurityOpt[1] != "no-new-privileges:true" || host.SecurityOpt[2] != "apparmor=unconfined" {
		t.Fatalf("security opts %v; want the seccomp profile and no-new-privileges kept, AppArmor added", host.SecurityOpt)
	}
	if len(host.CapAdd) != 1 || host.CapDrop[0] != "ALL" || host.Privileged {
		t.Fatalf("caps add %v drop %v privileged %t", host.CapAdd, host.CapDrop, host.Privileged)
	}
}

func TestLastLinePrefersTheExitingLine(t *testing.T) {
	logs := "time=1 level=INFO msg=ready\ntime=2 level=ERROR msg=exiting err=\"chromium: boom\"\n[1:1:0:ERROR:dbus.cc:1] draining\n"
	if got := lastLine([]byte(logs)); got != `time=2 level=ERROR msg=exiting err="chromium: boom"` {
		t.Fatalf("lastLine = %q", got)
	}
	if got := lastLine([]byte("a\nb\n")); got != "b" {
		t.Fatalf("without an exiting line: %q", got)
	}
}

func TestRemoteDaemon(t *testing.T) {
	// A unix socket can be forwarded from another machine too.
	for host, want := range map[string]bool{"": false, "unix:///run/user/1001/podman/podman.sock": true, "tcp://10.0.0.5:2376": true, "ssh://me@box": true} {
		t.Setenv("DOCKER_HOST", host)
		if got := RemoteDaemon(); got != want {
			t.Errorf("DOCKER_HOST=%q: remote %t", host, got)
		}
		if want && ubuntuUsernsRestriction() != "present" {
			t.Errorf("DOCKER_HOST=%q: a remote kernel must count as restricted", host)
		}
	}
}

func TestClientSpecCarriesTheAppArmorChoice(t *testing.T) {
	if got := (&Client{}).Spec(); got != Spec {
		t.Fatalf("no AppArmor option: %q", got)
	}
	unconfined, named := (&Client{apparmorOpt: "apparmor=unconfined"}).Spec(), (&Client{apparmorOpt: "apparmor=browserpier"}).Spec()
	if unconfined == Spec || named == Spec || unconfined == named {
		t.Fatalf("specs %q and %q don't tell the AppArmor choices apart", unconfined, named)
	}
}

func TestIsPodman(t *testing.T) {
	if !isPodman([]system.ComponentVersion{{Name: "Podman Engine"}}) {
		t.Fatal("Podman not recognised")
	}
	if isPodman([]system.ComponentVersion{{Name: "Engine"}, {Name: "containerd"}, {Name: "runc"}}) {
		t.Fatal("Docker taken for Podman")
	}
}

func TestMemoryInUse(t *testing.T) {
	if got := memoryInUse(container.MemoryStats{Usage: 1000, Stats: map[string]uint64{"inactive_file": 300}}); got != 700 {
		t.Errorf("cgroup v2: %d", got)
	}
	if got := memoryInUse(container.MemoryStats{Usage: 1000, Stats: map[string]uint64{"total_inactive_file": 100}}); got != 900 {
		t.Errorf("cgroup v1: %d", got)
	}
	if got := memoryInUse(container.MemoryStats{Usage: 1000}); got != 1000 {
		t.Errorf("no stats: %d", got)
	}
}
