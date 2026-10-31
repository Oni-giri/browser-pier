package config

import (
	"net"
	"strings"
	"testing"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestLoadDefaults(t *testing.T) {
	c, err := load(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != DefaultListen {
		t.Errorf("Listen = %q, want %q", c.Listen, DefaultListen)
	}
	if c.DataDir != DefaultDataDir {
		t.Errorf("DataDir = %q, want %q", c.DataDir, DefaultDataDir)
	}
}

func TestLoadFromEnv(t *testing.T) {
	c, err := load(env(map[string]string{
		"BROWSERPIER_LISTEN":        "127.0.0.1:9000",
		"BROWSERPIER_DATA_DIR":      "/srv/browserpier",
		"BROWSERPIER_HOST_DATA_DIR": "/home/u/bp",
		"BROWSERPIER_BROWSER_IMAGE": "example/chromium:test",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:9000" {
		t.Errorf("Listen = %q", c.Listen)
	}
	if c.DataDir != "/srv/browserpier" {
		t.Errorf("DataDir = %q", c.DataDir)
	}
	if c.HostDataDir != "/home/u/bp" || c.BrowserImage != "example/chromium:test" {
		t.Errorf("HostDataDir = %q, BrowserImage = %q", c.HostDataDir, c.BrowserImage)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for name, vars := range map[string]map[string]string{
		"listen without port": {"BROWSERPIER_LISTEN": "localhost"},
		"listen bad port":     {"BROWSERPIER_LISTEN": ":http-ish"},
		"listen port range":   {"BROWSERPIER_LISTEN": ":70000"},
		"relative host dir":   {"BROWSERPIER_HOST_DATA_DIR": "data"},
		"bad proxy":           {"BROWSERPIER_TRUSTED_PROXIES": "10.0.0.0/33"},
		"bad cookie mode":     {"BROWSERPIER_COOKIE_SECURE": "yes"},
		"bad public url":      {"BROWSERPIER_PUBLIC_URL": "pier.example.com"},
		"short password":      {"BROWSERPIER_ADMIN_PASSWORD": "short"},
		"bad files limit":     {"BROWSERPIER_FILES_LIMIT_MB": "-1"},
		"bad ram limit":       {"BROWSERPIER_RAM_LIMIT_PERCENT": "150"},
		"bad webrtc port":     {"BROWSERPIER_WEBRTC_PORT": "0"},
		"bad webrtc ip":       {"BROWSERPIER_WEBRTC_IPS": "pier.example"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(env(vars)); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}

func TestTrustedProxies(t *testing.T) {
	c, err := load(env(map[string]string{"BROWSERPIER_TRUSTED_PROXIES": "172.18.0.2, 10.1.0.0/16,private,::1"}))
	if err != nil {
		t.Fatal(err)
	}
	in := func(ip string) bool {
		for _, n := range c.TrustedProxies {
			if n.Contains(net.ParseIP(ip)) {
				return true
			}
		}
		return false
	}
	for _, ip := range []string{"172.18.0.2", "10.1.2.3", "192.168.1.1", "::1"} {
		if !in(ip) {
			t.Errorf("%s not trusted", ip)
		}
	}
	for _, ip := range []string{"100.64.0.1", "8.8.8.8"} { // Tailscale's range isn't private
		if in(ip) {
			t.Errorf("%s trusted", ip)
		}
	}
	if c, _ := load(env(nil)); len(c.TrustedProxies) != 0 || c.CookieSecure != "auto" || c.FilesLimitMB != 1024 || c.RAMLimitPercent != 70 || c.WebRTCPort != 7438 {
		t.Fatalf("defaults: %d trusted proxies, cookie %q", len(c.TrustedProxies), c.CookieSecure)
	}
}

func TestBadPasswordErrorHidesIt(t *testing.T) {
	_, err := load(env(map[string]string{"BROWSERPIER_ADMIN_PASSWORD": "s3cr"}))
	if err == nil || strings.Contains(err.Error(), "s3cr") {
		t.Fatalf("err = %v", err)
	}
}

// Compose passes optional settings through as empty strings: they mean unset.
func TestEmptyMeansUnset(t *testing.T) {
	c, err := load(env(map[string]string{"BROWSERPIER_RAM_LIMIT_PERCENT": "", "BROWSERPIER_LISTEN": "", "BROWSERPIER_ADMIN_PASSWORD": ""}))
	if err != nil {
		t.Fatal(err)
	}
	if c.RAMLimitPercent != 70 || c.RAMLimitFromEnv || c.Listen != DefaultListen || c.AdminPassword != "" {
		t.Fatalf("%+v", c)
	}
}
