// Package config reads the orchestrator's settings from BROWSERPIER_* environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	DefaultListen  = ":7437"
	DefaultDataDir = "./data"
)

// Config holds the orchestrator's settings.
type Config struct {
	// Listen is the TCP address the HTTP server binds to (BROWSERPIER_LISTEN).
	Listen string
	// DataDir holds the database and one folder per profile (BROWSERPIER_DATA_DIR).
	DataDir string
	// HostDataDir is DataDir's path on the Docker host, for bind mounts
	// (BROWSERPIER_HOST_DATA_DIR). Empty means detect it.
	HostDataDir string
	// BrowserImage overrides the browser image (BROWSERPIER_BROWSER_IMAGE).
	// Empty means the one matching this build.
	BrowserImage string
	// UsernsMode is the browser containers' user namespace mode
	// (BROWSERPIER_USERNS_MODE): "keep-id" under rootless Podman.
	UsernsMode string

	// AdminPassword, when set (BROWSERPIER_ADMIN_PASSWORD), is the admin
	// password; it wins over one set in the UI, at every start. Never log it.
	AdminPassword string
	// PublicURL is how users reach BrowserPier (BROWSERPIER_PUBLIC_URL),
	// e.g. https://pier.example.com. An https URL makes cookies Secure.
	PublicURL string
	// CookieSecure is auto, always or never (BROWSERPIER_COOKIE_SECURE).
	CookieSecure string
	// GeoIPURL is the echoip-style endpoint (JSON with country_iso and
	// time_zone) browsers use to find their exit location, through their
	// proxy (BROWSERPIER_GEOIP_URL; "off" turns it off).
	GeoIPURL string
	// FilesLimitMB caps each profile's downloads and uploads together, in
	// MiB (BROWSERPIER_FILES_LIMIT_MB); the oldest go first. 0 is no cap.
	FilesLimitMB int
	// TrustedProxies are the networks whose X-Forwarded-For and
	// X-Forwarded-Proto are believed (BROWSERPIER_TRUSTED_PROXIES: CIDRs,
	// comma-separated, or "private"). Empty trusts none.
	TrustedProxies []*net.IPNet
}

// privateNets is what BROWSERPIER_TRUSTED_PROXIES=private means: loopback
// and the private ranges Docker networks use.
var privateNets = []string{"127.0.0.0/8", "::1/128", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "fc00::/7"}

func parseProxies(v string) ([]*net.IPNet, error) {
	var out []*net.IPNet
	for _, part := range strings.Split(v, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		cidrs := []string{part}
		if part == "private" {
			cidrs = privateNets
		} else if !strings.Contains(part, "/") {
			if ip := net.ParseIP(part); ip != nil && ip.To4() != nil {
				part += "/32"
			} else {
				part += "/128"
			}
			cidrs = []string{part}
		}
		for _, c := range cidrs {
			_, n, err := net.ParseCIDR(c)
			if err != nil {
				return nil, fmt.Errorf("BROWSERPIER_TRUSTED_PROXIES: %q is not an address, a CIDR or \"private\"", c)
			}
			out = append(out, n)
		}
	}
	return out, nil
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		return def
	}
	c := Config{
		Listen:       get("BROWSERPIER_LISTEN", DefaultListen),
		DataDir:      get("BROWSERPIER_DATA_DIR", DefaultDataDir),
		HostDataDir:  get("BROWSERPIER_HOST_DATA_DIR", ""),
		BrowserImage: get("BROWSERPIER_BROWSER_IMAGE", ""),
		UsernsMode:   get("BROWSERPIER_USERNS_MODE", ""),
		// Read once and never echoed back in errors.
		AdminPassword: get("BROWSERPIER_ADMIN_PASSWORD", ""),
		PublicURL:     get("BROWSERPIER_PUBLIC_URL", ""),
		CookieSecure:  get("BROWSERPIER_COOKIE_SECURE", "auto"),
		GeoIPURL:      get("BROWSERPIER_GEOIP_URL", "https://ifconfig.co/json"),
	}

	_, port, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return Config{}, fmt.Errorf("BROWSERPIER_LISTEN: %w", err)
	}
	if _, err := strconv.ParseUint(port, 10, 16); err != nil {
		return Config{}, fmt.Errorf("BROWSERPIER_LISTEN: invalid port %q", port)
	}
	if c.DataDir == "" {
		return Config{}, errors.New("BROWSERPIER_DATA_DIR: must not be empty")
	}
	if c.GeoIPURL == "off" {
		c.GeoIPURL = ""
	} else if u, err := url.Parse(c.GeoIPURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return Config{}, fmt.Errorf("BROWSERPIER_GEOIP_URL: want an http(s) URL or off, got %q", c.GeoIPURL)
	}
	limit := get("BROWSERPIER_FILES_LIMIT_MB", "1024")
	if c.FilesLimitMB, err = strconv.Atoi(limit); err != nil || c.FilesLimitMB < 0 {
		return Config{}, fmt.Errorf("BROWSERPIER_FILES_LIMIT_MB: want a number of MiB (0 for no cap), got %q", limit)
	}
	if c.TrustedProxies, err = parseProxies(get("BROWSERPIER_TRUSTED_PROXIES", "")); err != nil {
		return Config{}, err
	}
	switch c.CookieSecure {
	case "auto", "always", "never":
	default:
		return Config{}, fmt.Errorf("BROWSERPIER_COOKIE_SECURE: want auto, always or never, got %q", c.CookieSecure)
	}
	if c.PublicURL != "" {
		u, err := url.Parse(c.PublicURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("BROWSERPIER_PUBLIC_URL: want http(s)://host[:port], got %q", c.PublicURL)
		}
	}
	if c.AdminPassword != "" && len(c.AdminPassword) < 8 {
		return Config{}, errors.New("BROWSERPIER_ADMIN_PASSWORD: use at least 8 characters")
	}
	if c.HostDataDir != "" && !filepath.IsAbs(c.HostDataDir) {
		return Config{}, fmt.Errorf("BROWSERPIER_HOST_DATA_DIR: want an absolute path, got %q", c.HostDataDir)
	}
	return c, nil
}
