package main

import (
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

const (
	display        = ":99"
	cdpAddr        = "127.0.0.1:9222"
	chromiumBinary = "/usr/lib/chromium/chromium"
)

// config is the container's settings, from BROWSERPIER_* environment variables
// set by the orchestrator.
type config struct {
	Width, Height int           // BROWSERPIER_SCREEN, e.g. 1920x1080
	ProfileDir    string        // BROWSERPIER_PROFILE_DIR, Chromium's user data dir
	ControlSocket string        // BROWSERPIER_CONTROL_SOCKET, the Unix socket the orchestrator connects to
	StopTimeout   time.Duration // BROWSERPIER_STOP_TIMEOUT, how long Chromium gets to close before it is killed
	// MountCheck (BROWSERPIER_MOUNT_CHECK), when set, must match the marker
	// file the orchestrator wrote into both mounted folders.
	MountCheck string
	// ProxyFile (BROWSERPIER_PROXY_FILE) holds the upstream proxy, if any;
	// it is read and deleted at start.
	ProxyFile string
	// GeoURL (BROWSERPIER_GEOIP_URL) is an echoip-style endpoint for the
	// exit location; empty turns the lookup off.
	GeoURL string
	// The location the profile ran with last time (BROWSERPIER_GEO_COUNTRY,
	// BROWSERPIER_GEO_TZ), and the profile's manual settings, which win
	// (BROWSERPIER_TZ_OVERRIDE, BROWSERPIER_LOCALE_OVERRIDE).
	StoredCountry, StoredTZ    string
	OverrideTZ, OverrideLocale string
	// LocalNetwork (BROWSERPIER_LOCAL_NETWORK) lets pages reach private
	// addresses: the LAN, the Docker host. Off by default.
	LocalNetwork bool
	// HostIPs (BROWSERPIER_HOST_IPS, comma separated) are the Docker
	// host's own addresses, which count as the local network.
	HostIPs []netip.Addr
	// BlockAds (BROWSERPIER_BLOCK_ADS) turns on the ad and tracker list,
	// read from BlockFile (BROWSERPIER_BLOCKLIST_FILE). Off by default.
	BlockAds  bool
	BlockFile string
	// Visual (BROWSERPIER_MODE=visual; api, the default, is false) turns
	// on Chromium's accessibility, published on the session bus, and the
	// tree reader, for Visual mode's element tree (PROJ-150). A session's mode is fixed when it
	// starts, so the state never changes during one.
	Visual bool
	// ChooserTimeout (BROWSERPIER_CHOOSER_TIMEOUT) is how long a page's
	// file chooser waits for an answer before it is cancelled.
	ChooserTimeout time.Duration
}

func loadConfig(lookup func(string) (string, bool)) (config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		return def
	}
	c := config{
		ProfileDir:     get("BROWSERPIER_PROFILE_DIR", "/profile"),
		ControlSocket:  get("BROWSERPIER_CONTROL_SOCKET", "/run/browserpier/control.sock"),
		MountCheck:     get("BROWSERPIER_MOUNT_CHECK", ""),
		ProxyFile:      get("BROWSERPIER_PROXY_FILE", "/run/browserpier/proxy.json"),
		BlockFile:      get("BROWSERPIER_BLOCKLIST_FILE", "/run/browserpier/blocklist.txt"),
		GeoURL:         get("BROWSERPIER_GEOIP_URL", ""),
		StoredCountry:  get("BROWSERPIER_GEO_COUNTRY", ""),
		StoredTZ:       get("BROWSERPIER_GEO_TZ", ""),
		OverrideTZ:     get("BROWSERPIER_TZ_OVERRIDE", ""),
		OverrideLocale: get("BROWSERPIER_LOCALE_OVERRIDE", ""),
	}

	screen := get("BROWSERPIER_SCREEN", "1920x1080")
	w, h, ok := strings.Cut(screen, "x")
	var err1, err2 error
	c.Width, err1 = strconv.Atoi(w)
	c.Height, err2 = strconv.Atoi(h)
	if !ok || err1 != nil || err2 != nil || c.Width < 640 || c.Height < 480 || c.Width > 7680 || c.Height > 4320 {
		return config{}, fmt.Errorf("BROWSERPIER_SCREEN: want WIDTHxHEIGHT between 640x480 and 7680x4320, got %q", screen)
	}

	var err error
	if c.LocalNetwork, err = strconv.ParseBool(get("BROWSERPIER_LOCAL_NETWORK", "false")); err != nil {
		return config{}, fmt.Errorf("BROWSERPIER_LOCAL_NETWORK: want true or false, got %q", get("BROWSERPIER_LOCAL_NETWORK", ""))
	}
	if c.BlockAds, err = strconv.ParseBool(get("BROWSERPIER_BLOCK_ADS", "false")); err != nil {
		return config{}, fmt.Errorf("BROWSERPIER_BLOCK_ADS: want true or false, got %q", get("BROWSERPIER_BLOCK_ADS", ""))
	}
	switch mode := get("BROWSERPIER_MODE", "api"); mode {
	case "api":
	case "visual":
		c.Visual = true
	default:
		return config{}, fmt.Errorf("BROWSERPIER_MODE: want api or visual, got %q", mode)
	}
	for _, s := range strings.Split(get("BROWSERPIER_HOST_IPS", ""), ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return config{}, fmt.Errorf("BROWSERPIER_HOST_IPS: %q is not an IP address", s)
		}
		c.HostIPs = append(c.HostIPs, ip)
	}
	if c.StopTimeout, err = time.ParseDuration(get("BROWSERPIER_STOP_TIMEOUT", "20s")); err != nil || c.StopTimeout <= 0 {
		return config{}, fmt.Errorf("BROWSERPIER_STOP_TIMEOUT: want a positive duration such as 20s, got %q", get("BROWSERPIER_STOP_TIMEOUT", ""))
	}
	if c.ChooserTimeout, err = time.ParseDuration(get("BROWSERPIER_CHOOSER_TIMEOUT", "5m")); err != nil || c.ChooserTimeout < time.Second || c.ChooserTimeout > time.Hour {
		return config{}, fmt.Errorf("BROWSERPIER_CHOOSER_TIMEOUT: want a duration between 1s and 1h, such as 5m, got %q", get("BROWSERPIER_CHOOSER_TIMEOUT", ""))
	}
	if c.ProfileDir == "" || c.ControlSocket == "" {
		return config{}, fmt.Errorf("BROWSERPIER_PROFILE_DIR and BROWSERPIER_CONTROL_SOCKET must not be empty")
	}
	return c, nil
}
