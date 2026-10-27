//go:build ignore

// gen_seccomp writes seccomp.json: Docker's default seccomp profile plus
// namespace creation, which Chromium's sandbox needs. Run it with
// `go generate ./images/chromium` after bumping profileURL.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
)

// Docker's default profile, pinned to a moby/profiles commit.
const profileURL = "https://raw.githubusercontent.com/moby/profiles/4b8214fc4971e06172938a1f1554ccff6647ee80/seccomp/default.json"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen_seccomp:", err)
		os.Exit(1)
	}
}

func run() error {
	resp, err := http.Get(profileURL)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: %s", profileURL, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	// Decode loosely so every field of Docker's profile survives unchanged.
	var profile map[string]any
	if err := json.Unmarshal(body, &profile); err != nil {
		return err
	}
	syscalls, ok := profile["syscalls"].([]any)
	if !ok {
		return fmt.Errorf("profile has no syscalls list")
	}
	profile["syscalls"] = append(syscalls, map[string]any{
		"names":   []string{"clone", "unshare"},
		"action":  "SCMP_ACT_ALLOW",
		"comment": "BrowserPier: Chromium's sandbox creates user, PID and network namespaces",
	})

	out, err := json.MarshalIndent(profile, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile("seccomp.json", append(out, '\n'), 0o644)
}
