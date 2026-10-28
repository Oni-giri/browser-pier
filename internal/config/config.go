// Package config reads the orchestrator's settings from BROWSERPIER_* environment variables.
package config

import (
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strconv"
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
	if c.HostDataDir != "" && !filepath.IsAbs(c.HostDataDir) {
		return Config{}, fmt.Errorf("BROWSERPIER_HOST_DATA_DIR: want an absolute path, got %q", c.HostDataDir)
	}
	return c, nil
}
