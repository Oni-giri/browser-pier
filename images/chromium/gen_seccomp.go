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
	profile["syscalls"] = append(syscalls, sandboxRules()...)

	out, err := json.MarshalIndent(profile, "", "\t")
	if err != nil {
		return err
	}
	return os.WriteFile("seccomp.json", append(out, '\n'), 0o644)
}

// Namespace flags from <linux/sched.h>.
const (
	cloneNewTime   = 0x00000080 // unshare only; in clone's flags this bit is part of the exit signal
	cloneNewNS     = 0x00020000
	cloneNewCgroup = 0x02000000
	cloneNewUTS    = 0x04000000
	cloneNewIPC    = 0x08000000
)

// sandboxRules allow clone and unshare to create the user, PID and network
// namespaces Chromium's sandbox uses, and no other kind. Each rule allows the
// call only when none of the other namespace flags is set.
func sandboxRules() []any {
	const comment = "BrowserPier: Chromium's sandbox creates user, PID and network namespaces, and nothing else"
	cloneDenied := uint64(cloneNewNS | cloneNewCgroup | cloneNewUTS | cloneNewIPC)
	// Docker's MASKED_EQ: allow when flags & value == valueTwo.
	rule := func(name string, index int, mask uint64, arches map[string]any) map[string]any {
		r := map[string]any{
			"names":   []string{name},
			"action":  "SCMP_ACT_ALLOW",
			"args":    []any{map[string]any{"index": index, "value": mask, "valueTwo": 0, "op": "SCMP_CMP_MASKED_EQ"}},
			"comment": comment,
		}
		for k, v := range arches {
			r[k] = v
		}
		return r
	}
	s390 := []string{"s390", "s390x"}
	return []any{
		// clone's flags are its first argument, except on s390 where they are the second.
		rule("clone", 0, cloneDenied, map[string]any{"excludes": map[string]any{"arches": s390}}),
		rule("clone", 1, cloneDenied, map[string]any{"includes": map[string]any{"arches": s390}}),
		rule("unshare", 0, cloneDenied|cloneNewTime, nil),
	}
}
