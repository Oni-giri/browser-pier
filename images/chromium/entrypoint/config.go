package main

import (
	"fmt"
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
}

func loadConfig(lookup func(string) (string, bool)) (config, error) {
	get := func(key, def string) string {
		if v, ok := lookup(key); ok {
			return v
		}
		return def
	}
	c := config{
		ProfileDir:    get("BROWSERPIER_PROFILE_DIR", "/profile"),
		ControlSocket: get("BROWSERPIER_CONTROL_SOCKET", "/run/browserpier/control.sock"),
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
	if c.StopTimeout, err = time.ParseDuration(get("BROWSERPIER_STOP_TIMEOUT", "20s")); err != nil || c.StopTimeout <= 0 {
		return config{}, fmt.Errorf("BROWSERPIER_STOP_TIMEOUT: want a positive duration such as 20s, got %q", get("BROWSERPIER_STOP_TIMEOUT", ""))
	}
	if c.ProfileDir == "" || c.ControlSocket == "" {
		return config{}, fmt.Errorf("BROWSERPIER_PROFILE_DIR and BROWSERPIER_CONTROL_SOCKET must not be empty")
	}
	return c, nil
}
