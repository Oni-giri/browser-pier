//go:build ignore

// gen_umbrel_hook copies apparmor/browserpier and apparmor/load into the
// Umbrel app's hooks/pre-start, as the bodies of its heredocs, leaving the
// rest of the hook as written. Umbrel's app update copies only the files
// it knows (hooks/ among them), so the hook carries them itself. Run it
// with `go generate -run gen_umbrel_hook ./images/chromium` after editing
// either; TestUmbrelHookCarriesTheAppArmorFiles fails until then.
package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
)

var hookPath = filepath.Join("..", "..", "umbrel", "yakitori-browserpier", "hooks", "pre-start")

// Each heredoc's delimiter, and the file it carries.
var heredocs = []struct{ delim, file string }{
	{"BROWSERPIER_PROFILE", filepath.Join("apparmor", "browserpier")},
	{"BROWSERPIER_LOADER", filepath.Join("apparmor", "load")},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "gen_umbrel_hook:", err)
		os.Exit(1)
	}
}

func run() error {
	hook, err := os.ReadFile(hookPath)
	if err != nil {
		return err
	}
	for _, h := range heredocs {
		body, err := os.ReadFile(h.file)
		if err != nil {
			return err
		}
		if hook, err = fill(hook, h.delim, body); err != nil {
			return fmt.Errorf("%s: %w", h.file, err)
		}
	}
	// WriteFile keeps the hook's mode: it must stay executable.
	return os.WriteFile(hookPath, hook, 0o755)
}

// fill replaces the body of the heredoc opened by <<'delim' with body.
func fill(hook []byte, delim string, body []byte) ([]byte, error) {
	if len(body) == 0 || body[len(body)-1] != '\n' {
		return nil, fmt.Errorf("must end with a newline")
	}
	end := []byte(delim + "\n")
	if bytes.HasPrefix(body, end) || bytes.Contains(body, append([]byte("\n"), end...)) {
		return nil, fmt.Errorf("has a line %q, which would end its heredoc early", delim)
	}
	lines := bytes.SplitAfter(hook, []byte("\n"))
	open := -1
	for i, l := range lines {
		if bytes.Contains(l, []byte("<<'"+delim+"'")) {
			if open >= 0 {
				return nil, fmt.Errorf("the hook opens <<'%s' twice", delim)
			}
			open = i
		}
	}
	if open < 0 {
		return nil, fmt.Errorf("the hook has no <<'%s'", delim)
	}
	for i := open + 1; i < len(lines); i++ {
		if bytes.Equal(lines[i], end) {
			var out []byte
			for _, l := range lines[:open+1] {
				out = append(out, l...)
			}
			out = append(out, body...)
			for _, l := range lines[i:] {
				out = append(out, l...)
			}
			return out, nil
		}
	}
	return nil, fmt.Errorf("the hook never closes <<'%s'", delim)
}
