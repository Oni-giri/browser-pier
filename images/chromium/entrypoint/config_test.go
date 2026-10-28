package main

import (
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
	want := config{Width: 1920, Height: 1080, ProfileDir: "/profile", ControlSocket: "/run/browserpier/control.sock", StopTimeout: 20 * time.Second}
	if c != want {
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
	}))
	if err != nil {
		t.Fatal(err)
	}
	want := config{Width: 1280, Height: 800, ProfileDir: "/p", ControlSocket: "/s/c.sock", StopTimeout: 45 * time.Second, MountCheck: "s1"}
	if c != want {
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
		"--window-size=1280,800",
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
