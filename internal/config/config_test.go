package config

import "testing"

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
		"empty listen":        {"BROWSERPIER_LISTEN": ""},
		"empty data dir":      {"BROWSERPIER_DATA_DIR": ""},
		"relative host dir":   {"BROWSERPIER_HOST_DATA_DIR": "data"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := load(env(vars)); err == nil {
				t.Error("want error, got nil")
			}
		})
	}
}
