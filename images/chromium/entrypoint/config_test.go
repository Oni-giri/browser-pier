package main

import (
	"net/netip"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	c, err := loadConfig(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	want := config{Width: 1920, Height: 1080, ProfileDir: "/profile", ControlSocket: "/run/browserpier/control.sock", StopTimeout: 20 * time.Second, ProxyFile: "/run/browserpier/proxy.json", BlockFile: "/run/browserpier/blocklist.txt", ChooserTimeout: 5 * time.Minute}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"BROWSERPIER_SCREEN":          "1280x800",
		"BROWSERPIER_PROFILE_DIR":     "/p",
		"BROWSERPIER_CONTROL_SOCKET":  "/s/c.sock",
		"BROWSERPIER_STOP_TIMEOUT":    "45s",
		"BROWSERPIER_CHOOSER_TIMEOUT": "3s",
		"BROWSERPIER_MOUNT_CHECK":     "s1",
		"BROWSERPIER_LOCAL_NETWORK":   "true",
		"BROWSERPIER_HOST_IPS":        "203.0.113.4, 2001:db8::4",
		"BROWSERPIER_BLOCK_ADS":       "true",
		"BROWSERPIER_BLOCKLIST_FILE":  "/s/list.txt",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config{Width: 1280, Height: 800, ProfileDir: "/p", ControlSocket: "/s/c.sock", StopTimeout: 45 * time.Second, MountCheck: "s1", ProxyFile: "/run/browserpier/proxy.json", LocalNetwork: true, BlockAds: true, BlockFile: "/s/list.txt", ChooserTimeout: 3 * time.Second,
		HostIPs: []netip.Addr{netip.MustParseAddr("203.0.113.4"), netip.MustParseAddr("2001:db8::4")}}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestLoadConfigRejectsBadValues(t *testing.T) {
	for _, vars := range []map[string]string{
		{"BROWSERPIER_SCREEN": "1920"},
		{"BROWSERPIER_SCREEN": "axb"},
		{"BROWSERPIER_SCREEN": "320x200"},
		{"BROWSERPIER_SCREEN": "10000x10000"},
		{"BROWSERPIER_STOP_TIMEOUT": "soon"},
		{"BROWSERPIER_STOP_TIMEOUT": "0s"},
		{"BROWSERPIER_CHOOSER_TIMEOUT": "10ms"},
		{"BROWSERPIER_CHOOSER_TIMEOUT": "2h"},
		{"BROWSERPIER_PROFILE_DIR": ""},
		{"BROWSERPIER_CONTROL_SOCKET": ""},
		{"BROWSERPIER_LOCAL_NETWORK": "lan"},
		{"BROWSERPIER_BLOCK_ADS": "some"},
		{"BROWSERPIER_HOST_IPS": "example.com"},
	} {
		if _, err := loadConfig(env(vars)); err == nil {
			t.Errorf("%v: want error, got nil", vars)
		}
	}
}

// TestChromiumEnv: Mesa starts no thread before Chromium's GPU sandbox
// does, which it can only do in a single-threaded process (PROJ-165).
func TestChromiumEnv(t *testing.T) {
	env := chromiumEnv([]string{"PATH=/usr/bin"}, nil, nil, false)
	for _, want := range []string{"LP_NUM_THREADS=0", "MESA_SHADER_CACHE_DISABLE=true"} {
		if !slices.Contains(env, want) {
			t.Errorf("Chromium's environment lacks %s: %v", want, env)
		}
	}
}

func TestChromiumArgs(t *testing.T) {
	c := config{Width: 1280, Height: 800, ProfileDir: "/profile"}
	args := chromiumArgs(c, false)
	for _, want := range []string{
		"--user-data-dir=/profile",
		"--password-store=basic",
		"--disable-blink-features=AutomationControlled",
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-component-update",
		"--disable-background-networking",
		"--no-first-run",
		"--window-position=0,0",
		"--window-size=1280,768", // above the 32px taskbar
		// WebGL on llvmpipe (PROJ-148).
		"--ignore-gpu-blocklist",
		"--disable-gpu-compositing",
		// The GPU process never runs without its own sandbox (PROJ-165).
		"--gpu-sandbox-failures-fatal=yes",
	} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, a := range args {
		for _, banned := range []string{"--enable-automation", "--headless", "--no-sandbox", "--user-agent",
			// No switch that takes the GPU process out of its own process
			// or drops a sandbox, and WebGL has one renderer, llvmpipe,
			// with no SwiftShader to fall back on.
			"--disable-gpu-sandbox", "--in-process-gpu", "--disable-seccomp-filter-sandbox",
			"--disable-gpu", "--use-gl=swiftshader", "--use-angle=swiftshader", "--enable-unsafe-swiftshader",
			// The GPU sandbox starts after GL, not before it, where GL
			// can't reach the X display (PROJ-165).
			"--gpu-sandbox-start-early",
		} {
			// A switch alone or with a value; --disable-gpu-compositing
			// isn't --disable-gpu.
			if a == banned || strings.HasPrefix(a, banned+"=") {
				t.Errorf("must not pass %s", a)
			}
		}
		if strings.Contains(strings.ToLower(a), "swiftshader") {
			t.Errorf("must not pass %s: WebGL runs on llvmpipe", a)
		}
	}
	for _, a := range args {
		if strings.HasPrefix(a, "--gpu-sandbox-failures-fatal=") && a != "--gpu-sandbox-failures-fatal=yes" {
			t.Errorf("%s: the GPU sandbox failing must be fatal", a)
		}
	}
	// One --disable-features, since Chromium reads only the last one, with
	// every feature the image turns off (PROJ-160); none enabled. Memory
	// isn't saved by weakening site isolation or the sandbox.
	var disable []string
	for _, a := range args {
		if f, ok := strings.CutPrefix(a, "--disable-features="); ok {
			disable = append(disable, f)
		}
		if strings.HasPrefix(a, "--enable-features") {
			t.Errorf("must not pass %s", a)
		}
		for _, banned := range []string{"--renderer-process-limit", "--single-process", "--js-flags",
			"--disable-site-isolation-trials", "--process-per-site"} {
			if a == banned || strings.HasPrefix(a, banned+"=") {
				t.Errorf("must not pass %s", a)
			}
		}
		if strings.Contains(a, "IsolateOrigins") || strings.Contains(a, "site-per-process") || strings.Contains(a, "SitePerProcess") {
			t.Errorf("must not pass %s: it weakens site isolation", a)
		}
	}
	if len(disable) != 1 {
		t.Fatalf("%d --disable-features switches, want exactly one: %q", len(disable), disable)
	}
	if got := strings.Split(disable[0], ","); !slices.Equal(got, []string{"BackForwardCache"}) {
		t.Errorf("--disable-features=%s, want BackForwardCache", disable[0])
	}
	if slices.Contains(args, "--disable-dev-shm-usage") {
		t.Error("--disable-dev-shm-usage without a small /dev/shm")
	}
	if !slices.Contains(chromiumArgs(c, true), "--disable-dev-shm-usage") {
		t.Error("small /dev/shm: want --disable-dev-shm-usage")
	}
}

func TestLoadConfigMode(t *testing.T) {
	for mode, visual := range map[string]bool{"": false, "api": false, "visual": true} {
		vars := map[string]string{}
		if mode != "" {
			vars["BROWSERPIER_MODE"] = mode
		}
		c, err := loadConfig(env(vars))
		if err != nil || c.Visual != visual {
			t.Errorf("BROWSERPIER_MODE=%q: visual %t, %v", mode, c.Visual, err)
		}
	}
	if _, err := loadConfig(env(map[string]string{"BROWSERPIER_MODE": "Visual"})); err == nil {
		t.Error("BROWSERPIER_MODE=Visual: want an error")
	}
}

// TestChromiumArgsByMode: API mode's command line and environment are
// what they are without PROJ-150; Visual mode's differ by the
// accessibility switch and the two variables that point it at the session
// bus only.
func TestChromiumArgsByMode(t *testing.T) {
	api := config{Width: 1280, Height: 800, ProfileDir: "/profile"}
	before := chromiumArgs(api, false)
	if slices.ContainsFunc(before, func(a string) bool { return strings.Contains(a, "accessibility") }) {
		t.Fatalf("API mode has accessibility: %q", before)
	}
	where := geo{TimeZone: "Europe/Riga", Locale: "lv-LV"}
	bus := &sessionBus{dir: "/tmp/browserpier-bus-1000"}
	envBefore := []string{"DISPLAY=:99", "TZ=Europe/Riga", "LANGUAGE=lv_LV:lv", "LANG=lv_LV.UTF-8",
		"DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/browserpier-bus-1000/bus", "LP_NUM_THREADS=0", "MESA_SHADER_CACHE_DISABLE=true"}
	if got := chromiumEnv(nil, where.env(), bus, false); !slices.Equal(got, envBefore) {
		t.Fatalf("API mode's environment: %q", got)
	}

	vis := api
	vis.Visual = true
	if got := chromiumArgs(vis, false); !slices.Equal(got, append(slices.Clone(before), "--force-renderer-accessibility=basic")) {
		t.Fatalf("Visual mode: %q", got)
	}
	got := chromiumEnv(nil, where.env(), bus, true)
	if want := append(slices.Clone(envBefore), "AT_SPI_BUS_ADDRESS=unix:path=/tmp/browserpier-bus-1000/bus", "ACCESSIBILITY_ENABLED=1"); !slices.Equal(got, want) {
		t.Fatalf("Visual mode's environment: %q", got)
	}
}
