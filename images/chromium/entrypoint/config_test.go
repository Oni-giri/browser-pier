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
	want := config{Width: 1920, Height: 1080, ProfileDir: "/profile", ControlSocket: "/run/browserpier/control.sock", StopTimeout: 20 * time.Second, ProxyFile: "/run/browserpier/proxy.json", BlockFile: "/run/browserpier/blocklist.txt"}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v, want %+v", c, want)
	}
}

func TestLoadConfigFromEnv(t *testing.T) {
	c, err := loadConfig(env(map[string]string{
		"BROWSERPIER_SCREEN":         "1280x800",
		"BROWSERPIER_PROFILE_DIR":    "/p",
		"BROWSERPIER_CONTROL_SOCKET": "/s/c.sock",
		"BROWSERPIER_STOP_TIMEOUT":   "45s",
		"BROWSERPIER_MOUNT_CHECK":    "s1",
		"BROWSERPIER_LOCAL_NETWORK":  "true",
		"BROWSERPIER_HOST_IPS":       "203.0.113.4, 2001:db8::4",
		"BROWSERPIER_BLOCK_ADS":      "true",
		"BROWSERPIER_BLOCKLIST_FILE": "/s/list.txt",
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config{Width: 1280, Height: 800, ProfileDir: "/p", ControlSocket: "/s/c.sock", StopTimeout: 45 * time.Second, MountCheck: "s1", ProxyFile: "/run/browserpier/proxy.json", LocalNetwork: true, BlockAds: true, BlockFile: "/s/list.txt",
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
	} {
		if !slices.Contains(args, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, a := range args {
		for _, banned := range []string{"--enable-automation", "--headless", "--no-sandbox", "--user-agent"} {
			if strings.HasPrefix(a, banned) {
				t.Errorf("must not pass %s", a)
			}
		}
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
// what they were before PROJ-150, byte for byte; Visual mode's differ by
// the accessibility switch and the bus's two variables only.
func TestChromiumArgsByMode(t *testing.T) {
	api := config{Width: 1280, Height: 800, ProfileDir: "/profile"}
	before := []string{
		"--user-data-dir=/profile",
		"--remote-debugging-port=9222",
		"--password-store=basic",
		"--disable-blink-features=AutomationControlled",
		"--use-angle=swiftshader",
		"--enable-unsafe-swiftshader",
		"--proxy-server=socks5://127.0.0.1:1080",
		"--proxy-bypass-list=<-loopback>",
		"--host-resolver-rules=MAP * ~NOTFOUND , EXCLUDE 127.0.0.1",
		"--webrtc-ip-handling-policy=disable_non_proxied_udp",
		"--disable-component-update",
		"--disable-background-networking",
		"--no-first-run",
		"--no-default-browser-check",
		"--hide-crash-restore-bubble",
		"--noerrdialogs",
		"--window-position=0,0",
		"--window-size=1280,768",
	}
	if got := chromiumArgs(api, false); !slices.Equal(got, before) {
		t.Fatalf("API mode:\n got  %q\n want %q", got, before)
	}
	where := geo{TimeZone: "Europe/Riga", Locale: "lv-LV"}
	envBefore := []string{"DISPLAY=:99", "TZ=Europe/Riga", "LANGUAGE=lv_LV:lv", "LANG=lv_LV.UTF-8"}
	if got := chromiumEnv(api, where, ""); !slices.Equal(got, envBefore) {
		t.Fatalf("API mode's environment: %q", got)
	}

	vis := api
	vis.Visual = true
	if got := chromiumArgs(vis, false); !slices.Equal(got, append(slices.Clone(before), "--force-renderer-accessibility")) {
		t.Fatalf("Visual mode: %q", got)
	}
	got := chromiumEnv(vis, where, "unix:path=/tmp/browserpier-a11y-1000/bus")
	if want := append(slices.Clone(envBefore), "AT_SPI_BUS_ADDRESS=unix:path=/tmp/browserpier-a11y-1000/bus", "ACCESSIBILITY_ENABLED=1"); !slices.Equal(got, want) {
		t.Fatalf("Visual mode's environment: %q", got)
	}
}
