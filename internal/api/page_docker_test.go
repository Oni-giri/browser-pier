//go:build docker

package api

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/fence/fencetest"
)

// startAPIBrowser starts an API-mode browser whose pages may reach the
// test sites.
func (a agent) startAPIBrowser(t *testing.T, prefix string) sessionJSON {
	t.Helper()
	name := fmt.Sprintf("%s%d", prefix, time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	if code := a.do(http.MethodPost, "/api/profiles", fmt.Sprintf(`{"name": %q, "default_mode": "api"}`, name), nil); code != http.StatusCreated {
		t.Fatalf("create profile: %d", code)
	}
	a.allowLocalNetwork(t, name)
	var s sessionJSON
	// It blocks ads and trackers, as new profiles do (PROJ-119).
	if code := a.do(http.MethodPost, "/api/sessions", fmt.Sprintf(`{"profile": %q}`, name), &s); code != http.StatusCreated || !s.BlockAds {
		t.Fatalf("start: %d %+v", code, s)
	}
	return s
}

// tool calls a page tool, returning the status and the text or error.
func (a agent) tool(id, name string, body any) (int, string) {
	a.t.Helper()
	raw, _ := json.Marshal(body)
	req, _ := http.NewRequest(http.MethodPost, a.base+"/api/sessions/"+id+"/"+name, strings.NewReader(string(raw)))
	req.Header.Set("Authorization", "Bearer "+a.token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		a.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	var out struct{ Text, Error string }
	_ = json.Unmarshal(b, &out)
	if resp.StatusCode == http.StatusOK {
		return resp.StatusCode, out.Text
	}
	return resp.StatusCode, out.Error
}

// mustTool calls a page tool that must work.
func (a agent) mustTool(id, name string, body any) string {
	a.t.Helper()
	code, text := a.tool(id, name, body)
	if code != http.StatusOK {
		a.t.Fatalf("%s %v: %d %s", name, body, code, text)
	}
	return text
}

// openBuilt navigates to a page that a script builds as it loads, and
// waits until the script has finished, which its title says. Building a
// stress page can keep Chrome busy past navigate's quick check, which
// then rightly answers 504 (the page is busy); the page goes on loading,
// so a 504 is accepted and the page polled, with snapshot {max_chars: 0},
// for up to 90 s. Whatever the test then measures starts on a built page.
func (a agent) openBuilt(id, url, title string) {
	a.t.Helper()
	start := time.Now()
	navCode, text := a.tool(id, "navigate", map[string]string{"url": url})
	if navCode != http.StatusOK && navCode != http.StatusGatewayTimeout {
		a.t.Fatalf("navigate %s: %d %s", url, navCode, text)
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		code, text := a.tool(id, "snapshot", map[string]any{"max_chars": 0})
		if e, err := fencetest.Page(text); code == http.StatusOK && err == nil && e.URL == url && e.Title == title {
			if navCode != http.StatusOK {
				a.t.Logf("navigate %s: %d; the page was built after %v", url, navCode, time.Since(start).Round(time.Millisecond))
			}
			return
		}
		if code != http.StatusOK && code != http.StatusGatewayTimeout {
			a.t.Fatalf("snapshot of %s: %d %s", url, code, text)
		}
		if time.Now().After(deadline) {
			a.t.Fatalf("%s wasn't built after 90 s: %d %s", url, code, text)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// refFor finds the ref on the snapshot line naming what.
func refFor(t *testing.T, snapshot, what string) string {
	t.Helper()
	for _, l := range strings.Split(snapshot, "\n") {
		if strings.Contains(l, what) {
			if m := regexp.MustCompile(`^([a-z][a-z0-9]e[0-9]+) `).FindStringSubmatch(l); m != nil {
				return m[1]
			}
		}
	}
	t.Fatalf("no ref for %q in\n%s", what, snapshot)
	return ""
}
