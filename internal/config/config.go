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

	"github.com/Oni-giri/browser-pier/internal/blocklist"
	"github.com/pion/stun/v4"
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
	// BrowserAppArmor names the browser containers' AppArmor profile
	// (BROWSERPIER_BROWSER_APPARMOR). Empty means BrowserPier picks one:
	// Docker's docker-default may refuse Chromium's sandbox on AppArmor 4.
	BrowserAppArmor string

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
	// BlocklistURLs are the ad and tracker lists browsers block from
	// (BROWSERPIER_BLOCKLIST_URLS, comma or space separated; "none" for
	// none). Unset: blocklist.DefaultURLs.
	BlocklistURLs []string
	// WebRTCPort is the one UDP port (and TCP port, as a fallback) live
	// views use (BROWSERPIER_WEBRTC_PORT).
	WebRTCPort int
	// WebRTCIPs are the addresses advertised to live viewers: a VPS's
	// public IP or the Tailscale IP (BROWSERPIER_WEBRTC_IPS, comma
	// separated). Empty advertises every local address.
	WebRTCIPs []string
	// TURNURLs are the TURN servers live viewers may relay through, on
	// networks that block UDP or direct connections (BROWSERPIER_TURN_URLS,
	// comma separated), e.g. turn:pier.example.com:3478?transport=tcp.
	TURNURLs []string
	// TURNSecret is the TURN REST API's shared secret
	// (BROWSERPIER_TURN_SECRET; coturn's static-auth-secret), from which
	// each viewer gets credentials that expire. Never log it.
	TURNSecret string
	// TURNUsername and TURNPassword are one static TURN login that every
	// viewer gets (BROWSERPIER_TURN_USERNAME, BROWSERPIER_TURN_PASSWORD),
	// for services without the REST API. Never log the password.
	TURNUsername, TURNPassword string
	// TURNPeerIPs are this server's addresses as the TURN server reaches
	// it, advertised besides WebRTCIPs (BROWSERPIER_TURN_PEER_IPS): the
	// compose network's address, for the bundled coturn.
	TURNPeerIPs []string
	// RAMLimitPercent caps the memory all browsers may use together, as a
	// share of the host's (BROWSERPIER_RAM_LIMIT_PERCENT, default 70; 0 for
	// no cap). Past it, the longest-idle unused browser is stopped to make
	// room, or a new one is refused.
	RAMLimitPercent int
	// RAMLimitFromEnv: BROWSERPIER_RAM_LIMIT_PERCENT is set. Otherwise the
	// limit saved in the web UI's settings wins over the default.
	RAMLimitFromEnv bool
	// FilesLimitMB caps each profile's downloads and uploads together, in
	// MiB (BROWSERPIER_FILES_LIMIT_MB); the oldest go first. 0 is no cap.
	FilesLimitMB int
	// StopBrowsersOnExit stops every browser when the orchestrator stops
	// (BROWSERPIER_STOP_BROWSERS_ON_EXIT), as Umbrel expects of an app.
	// Otherwise they keep running for the next one, through an update.
	StopBrowsersOnExit bool
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

func parseIPs(key, v string) ([]string, error) {
	var out []string
	for _, ip := range strings.Split(v, ",") {
		if ip = strings.TrimSpace(ip); ip == "" {
			continue
		}
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("%s: %q is not an IP address", key, ip)
		}
		out = append(out, ip)
	}
	return out, nil
}

// minTURNSecret is the shortest shared secret taken: anyone given
// credentials can test guesses at it offline, and one found mints more.
const minTURNSecret = 16

// loadTURN reads the TURN settings. Errors never echo the secret or the
// password.
func (c *Config) loadTURN(get func(key, def string) string) error {
	for _, u := range strings.Split(get("BROWSERPIER_TURN_URLS", ""), ",") {
		if u = strings.TrimSpace(u); u == "" {
			continue
		}
		uri, err := stun.ParseURI(u)
		if err != nil || uri.Host == "" {
			return fmt.Errorf("BROWSERPIER_TURN_URLS: %q isn't a turn:, turns:, stun: or stuns: URL", u)
		}
		c.TURNURLs = append(c.TURNURLs, u)
	}
	c.TURNSecret = get("BROWSERPIER_TURN_SECRET", "")
	c.TURNUsername = get("BROWSERPIER_TURN_USERNAME", "")
	c.TURNPassword = get("BROWSERPIER_TURN_PASSWORD", "")
	var err error
	if c.TURNPeerIPs, err = parseIPs("BROWSERPIER_TURN_PEER_IPS", get("BROWSERPIER_TURN_PEER_IPS", "")); err != nil {
		return err
	}
	static := c.TURNUsername != "" || c.TURNPassword != ""
	switch {
	case c.TURNSecret != "" && static:
		return errors.New("BROWSERPIER_TURN_SECRET and BROWSERPIER_TURN_USERNAME/PASSWORD: set one or the other")
	case static && (c.TURNUsername == "" || c.TURNPassword == ""):
		return errors.New("BROWSERPIER_TURN_USERNAME and BROWSERPIER_TURN_PASSWORD: set both")
	case c.TURNSecret != "" && len(c.TURNSecret) < minTURNSecret:
		return fmt.Errorf("BROWSERPIER_TURN_SECRET: use at least %d characters (openssl rand -hex 32)", minTURNSecret)
	case (c.TURNSecret != "" || static) && len(c.TURNURLs) == 0:
		return errors.New("BROWSERPIER_TURN_URLS: set it to the TURN server's URLs, or unset the TURN credentials")
	}
	if c.TURNSecret == "" && !static {
		for _, u := range c.TURNURLs {
			if strings.HasPrefix(u, "turn:") || strings.HasPrefix(u, "turns:") {
				// Browsers refuse a TURN URL without credentials, and a
				// TURN server that asks for none relays for anyone.
				return errors.New("BROWSERPIER_TURN_URLS: a TURN server needs BROWSERPIER_TURN_SECRET (or BROWSERPIER_TURN_USERNAME and BROWSERPIER_TURN_PASSWORD)")
			}
		}
	}
	return nil
}

// Load reads the configuration from the process environment.
func Load() (Config, error) {
	return load(os.LookupEnv)
}

func load(lookup func(string) (string, bool)) (Config, error) {
	// An empty variable counts as unset: compose files pass optional
	// settings through as empty strings.
	set := func(key string) (string, bool) {
		v, ok := lookup(key)
		return v, ok && v != ""
	}
	get := func(key, def string) string {
		if v, ok := set(key); ok {
			return v
		}
		return def
	}
	c := Config{
		Listen:          get("BROWSERPIER_LISTEN", DefaultListen),
		DataDir:         get("BROWSERPIER_DATA_DIR", DefaultDataDir),
		HostDataDir:     get("BROWSERPIER_HOST_DATA_DIR", ""),
		BrowserImage:    get("BROWSERPIER_BROWSER_IMAGE", ""),
		UsernsMode:      get("BROWSERPIER_USERNS_MODE", ""),
		BrowserAppArmor: get("BROWSERPIER_BROWSER_APPARMOR", ""),
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
	bl, blSet := set("BROWSERPIER_BLOCKLIST_URLS")
	c.BlocklistURLs = blocklist.ParseURLs(bl, blSet)
	for _, s := range c.BlocklistURLs {
		if u, err := url.Parse(s); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return Config{}, fmt.Errorf("BROWSERPIER_BLOCKLIST_URLS: want http(s) URLs or none, got %q", s)
		}
	}
	rtcPort := get("BROWSERPIER_WEBRTC_PORT", "7438")
	if c.WebRTCPort, err = strconv.Atoi(rtcPort); err != nil || c.WebRTCPort < 1 || c.WebRTCPort > 65535 {
		return Config{}, fmt.Errorf("BROWSERPIER_WEBRTC_PORT: want a port number, got %q", rtcPort)
	}
	if c.WebRTCIPs, err = parseIPs("BROWSERPIER_WEBRTC_IPS", get("BROWSERPIER_WEBRTC_IPS", "")); err != nil {
		return Config{}, err
	}
	if err := c.loadTURN(get); err != nil {
		return Config{}, err
	}
	ram := get("BROWSERPIER_RAM_LIMIT_PERCENT", "70")
	_, c.RAMLimitFromEnv = set("BROWSERPIER_RAM_LIMIT_PERCENT")
	if c.RAMLimitPercent, err = strconv.Atoi(strings.TrimSuffix(ram, "%")); err != nil || c.RAMLimitPercent < 0 || c.RAMLimitPercent > 100 {
		return Config{}, fmt.Errorf("BROWSERPIER_RAM_LIMIT_PERCENT: want 0 to 100, got %q", ram)
	}
	limit := get("BROWSERPIER_FILES_LIMIT_MB", "1024")
	if c.FilesLimitMB, err = strconv.Atoi(limit); err != nil || c.FilesLimitMB < 0 {
		return Config{}, fmt.Errorf("BROWSERPIER_FILES_LIMIT_MB: want a number of MiB (0 for no cap), got %q", limit)
	}
	stopOnExit := get("BROWSERPIER_STOP_BROWSERS_ON_EXIT", "false")
	if c.StopBrowsersOnExit, err = strconv.ParseBool(stopOnExit); err != nil {
		return Config{}, fmt.Errorf("BROWSERPIER_STOP_BROWSERS_ON_EXIT: want true or false, got %q", stopOnExit)
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
