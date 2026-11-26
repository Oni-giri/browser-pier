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
	if c.StopBrowsersOnExit {
		t.Error("browsers stop on exit by default")
	}
}

func TestLoadFromEnv(t *testing.T) {
	c, err := load(env(map[string]string{
		"BROWSERPIER_LISTEN":                "127.0.0.1:9000",
		"BROWSERPIER_DATA_DIR":              "/srv/browserpier",
		"BROWSERPIER_HOST_DATA_DIR":         "/home/u/bp",
		"BROWSERPIER_BROWSER_IMAGE":         "example/chromium:test",
		"BROWSERPIER_STOP_BROWSERS_ON_EXIT": "true",
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
	if !c.StopBrowsersOnExit {
		t.Error("StopBrowsersOnExit not set")
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
		"bad stop on exit":    {"BROWSERPIER_STOP_BROWSERS_ON_EXIT": "sometimes"},
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

func TestTURN(t *testing.T) {
	c, err := load(env(map[string]string{
		"BROWSERPIER_TURN_URLS":     "turn:pier.example.com:3478?transport=udp, turn:pier.example.com:3478?transport=tcp,turns:turn.example.net:443",
		"BROWSERPIER_TURN_SECRET":   "0123456789abcdef0123",
		"BROWSERPIER_TURN_PEER_IPS": "10.247.74.2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.TURNURLs) != 3 || c.TURNURLs[1] != "turn:pier.example.com:3478?transport=tcp" || c.TURNSecret != "0123456789abcdef0123" || len(c.TURNPeerIPs) != 1 {
		t.Fatalf("TURN: %q %q", c.TURNURLs, c.TURNPeerIPs)
	}
	c, err = load(env(map[string]string{"BROWSERPIER_TURN_URLS": "turns:turn.example.net:443", "BROWSERPIER_TURN_USERNAME": "pier", "BROWSERPIER_TURN_PASSWORD": "pw"}))
	if err != nil || c.TURNUsername != "pier" || c.TURNPassword != "pw" {
		t.Fatalf("static login: %v", err)
	}
	// A STUN server needs no credentials.
	if _, err := load(env(map[string]string{"BROWSERPIER_TURN_URLS": "stun:stun.example.net:3478"})); err != nil {
		t.Fatal(err)
	}
	if c, _ := load(env(nil)); len(c.TURNURLs) != 0 || c.TURNSecret != "" {
		t.Fatalf("TURN is on by default: %q", c.TURNURLs)
	}
}

func TestTURNRejectsBadValues(t *testing.T) {
	const secret = "s3cret-s3cret-s3cret"
	for name, vars := range map[string]map[string]string{
		"not a turn url":       {"BROWSERPIER_TURN_URLS": "https://pier.example.com", "BROWSERPIER_TURN_SECRET": secret},
		"no host":              {"BROWSERPIER_TURN_URLS": "turn:", "BROWSERPIER_TURN_SECRET": secret},
		"one bad url":          {"BROWSERPIER_TURN_URLS": "turn:x:3478,nope", "BROWSERPIER_TURN_SECRET": secret},
		"turn without login":   {"BROWSERPIER_TURN_URLS": "turn:pier.example.com:3478"},
		"secret without urls":  {"BROWSERPIER_TURN_SECRET": secret},
		"login without urls":   {"BROWSERPIER_TURN_USERNAME": "pier", "BROWSERPIER_TURN_PASSWORD": secret},
		"secret and login":     {"BROWSERPIER_TURN_URLS": "turn:x:3478", "BROWSERPIER_TURN_SECRET": secret, "BROWSERPIER_TURN_USERNAME": "pier", "BROWSERPIER_TURN_PASSWORD": secret},
		"username alone":       {"BROWSERPIER_TURN_URLS": "turn:x:3478", "BROWSERPIER_TURN_USERNAME": "pier"},
		"password alone":       {"BROWSERPIER_TURN_URLS": "turn:x:3478", "BROWSERPIER_TURN_PASSWORD": secret},
		"short secret":         {"BROWSERPIER_TURN_URLS": "turn:x:3478", "BROWSERPIER_TURN_SECRET": "s3cret"},
		"bad peer ip":          {"BROWSERPIER_TURN_URLS": "turn:x:3478", "BROWSERPIER_TURN_SECRET": secret, "BROWSERPIER_TURN_PEER_IPS": "browserpier"},
		"password and short s": {"BROWSERPIER_TURN_URLS": "turn:x:3478", "BROWSERPIER_TURN_SECRET": "s3cret", "BROWSERPIER_TURN_PASSWORD": "s3cret-pw"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := load(env(vars))
			if err == nil {
				t.Fatal("want error, got nil")
			}
			// Errors never echo a secret or a password.
			for _, k := range []string{"BROWSERPIER_TURN_SECRET", "BROWSERPIER_TURN_PASSWORD"} {
				if v := vars[k]; v != "" && strings.Contains(err.Error(), v) {
					t.Fatalf("%s is in the error: %v", k, err)
				}
			}
		})
	}
}

// PROJ-159: browsers get the daemon's own user namespace, or Podman's
// keep-id; anything else is refused at start-up, naming the variable.
func TestUsernsMode(t *testing.T) {
	for _, v := range []string{"", "keep-id", "keep-id:uid=1000,gid=1000", "keep-id:size=65536", "keep-id:uid=1000,gid=1000,size=65536"} {
		c, err := load(env(map[string]string{"BROWSERPIER_USERNS_MODE": v}))
		if err != nil || c.UsernsMode != v {
			t.Errorf("%q: %q, %v", v, c.UsernsMode, err)
		}
	}
	for _, v := range []string{"host", "private", "auto", "nomap", "ns:/proc/1/ns/user", "keep-id:", "keep-id:uid=x", "keep-id;host", " keep-id", "Keep-id",
		"keep-id:typo=1000", "keep-id:uid=1000,typo=1", "keep-id:uid=1,uid=2", "keep-id:uid=4294967296"} {
		_, err := load(env(map[string]string{"BROWSERPIER_USERNS_MODE": v}))
		if err == nil || !strings.HasPrefix(err.Error(), "BROWSERPIER_USERNS_MODE: ") {
			t.Errorf("%q: %v", v, err)
		}
	}
}
