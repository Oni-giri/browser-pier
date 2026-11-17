//go:build docker && measure

package mcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/browsertest"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/profiles"
	"github.com/Oni-giri/browser-pier/internal/sessions"
)

// TestMeasureTextAfterShortcutThroughBrowserPier is PROJ-151's measure
// through the orchestrator: the three calls (ctrl+l, type the /arrived
// URL, Return) back to back over REST (/computer) and over MCP
// (press_keys, type_text), on a /keys page whose key handler takes 0 or
// 200 ms, to see how much the orchestrator's own round trip hides. The
// control socket's numbers are images/chromium's TestMeasureTextAfterShortcut.
//
//	go test -tags 'docker measure' -run TestMeasureTextAfterShortcutThroughBrowserPier -v ./internal/mcp/
//
// MEASURE_RUNS sets the runs per shape at instant pace (30), and
// MEASURE_HUMAN_RUNS at human pace, the default (10).
func TestMeasureTextAfterShortcutThroughBrowserPier(t *testing.T) {
	image := os.Getenv("BROWSERPIER_TEST_IMAGE")
	if image == "" {
		t.Skip("BROWSERPIER_TEST_IMAGE not set")
	}
	ctx := context.Background()
	rt, err := docker.New(docker.Options{Image: image, UsernsMode: os.Getenv("BROWSERPIER_USERNS_MODE"), Instance: fmt.Sprintf("test%d", time.Now().UnixNano())})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	dir := t.TempDir()
	if err := rt.UseDataDir(ctx, dir, dir); err != nil {
		t.Fatal(err)
	}
	d, err := db.Open(ctx, filepath.Join(dir, "browserpier.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	log := slog.New(slog.DiscardHandler)
	m, err := sessions.NewManager(d, rt, sessions.Layout{DataDir: dir, HostDataDir: dir, UID: os.Getuid(), GID: os.Getgid()}, log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, s := range m.List() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			_, _ = m.Stop(ctx, s.ID)
			cancel()
		}
		m.Close()
	})
	name := fmt.Sprintf("k%d", time.Now().UnixNano()%1e9)
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", "browserpier-"+name).Run() })
	p := profiles.New(name)
	p.Width, p.Height = 1280, 800
	p.LocalNetwork = true
	if _, err := m.Profiles().Create(ctx, p); err != nil {
		t.Fatal(err)
	}
	tokens := auth.NewTokens(d)
	_, secret, err := tokens.Create(ctx, "measure", true, nil)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := auth.NewAdmin(ctx, d, "the-admin-password", log)
	if err != nil {
		t.Fatal(err)
	}
	web := auth.NewWeb(admin, tokens, log, auth.WebOptions{CookieSecure: "auto"})
	a := api.New(m, web, files.New(m.Layout().FilesDir, 0, os.Getuid(), os.Getgid()), log, "")
	routes, mux := http.NewServeMux(), http.NewServeMux()
	a.Register(routes)
	a.Register(mux)
	New(routes, web, log, "test").Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	ks := browsertest.NewKeySite(t, browsertest.SiteHost(t))
	f := &fixture{t: t, srv: srv}
	cs := f.connect(secret, versions[0], nil)
	call(t, cs, "start_browser", map[string]any{"profile": name})
	live, _ := m.Live(name)
	id := live.ID

	rest := func(body string) {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/sessions/"+id+"/computer", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+secret)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		msg, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %s", body, resp.StatusCode, msg)
		}
	}
	paced := func(pace string) string {
		if pace == "" {
			return ""
		}
		return fmt.Sprintf(`, "pace": %q`, pace)
	}
	type via struct {
		key, typ func(s, pace string)
	}
	vias := map[string]via{
		"REST": {
			key: func(k, pace string) { rest(fmt.Sprintf(`{"action": "key", "keys": %q%s}`, k, paced(pace))) },
			typ: func(s, pace string) { rest(fmt.Sprintf(`{"action": "type", "text": %q%s}`, s, paced(pace))) },
		},
		"MCP": {
			key: func(k, pace string) {
				args := map[string]any{"session_id": id, "keys": k}
				if pace != "" {
					args["pace"] = pace
				}
				if res, out := call(t, cs, "press_keys", args); res.IsError {
					t.Fatalf("press_keys %s: %s", k, out)
				}
			},
			typ: func(s, pace string) {
				args := map[string]any{"session_id": id, "text": s}
				if pace != "" {
					args["pace"] = pace
				}
				if res, out := call(t, cs, "type_text", args); res.IsError {
					t.Fatalf("type_text: %s", out)
				}
			},
		},
	}

	seq := 0
	nextID := func() string { seq++; return "m" + strconv.Itoa(seq) }
	cur := ""
	slow := 0
	quiet := func(n string) {
		last, since := -1, time.Now()
		for deadline := time.Now().Add(2 * time.Minute); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
			if got := len(ks.Run(n).Keys); got != last {
				last, since = got, time.Now()
			} else if time.Since(since) > 800*time.Millisecond {
				return
			}
		}
	}
	reset := func() {
		t.Helper()
		if cur != "" {
			quiet(cur)
		}
		n := nextID()
		for range 3 {
			rest(`{"action": "click", "x": 500, "y": 63, "pace": "instant"}`)
			time.Sleep(300 * time.Millisecond)
			rest(`{"action": "key", "keys": "ctrl+a", "pace": "instant"}`)
			time.Sleep(200 * time.Millisecond)
			rest(fmt.Sprintf(`{"action": "type", "text": %q, "pace": "instant"}`, ks.Keys(n, slow, "field")))
			time.Sleep(200 * time.Millisecond)
			rest(`{"action": "key", "keys": "Return", "pace": "instant"}`)
			if _, ok := ks.WaitRun(n, 15*time.Second, func(r browsertest.KeyRun) bool { return r.Ready }); ok {
				cur = n
				return
			}
			quiet(n)
		}
		t.Fatal("the /keys page never loaded")
	}
	seen := func(n string) int {
		c := 0
		for _, e := range ks.Run(n).Keys {
			if !e.Ctrl && e.Key != "Control" && e.Key != "Shift" {
				c++
			}
		}
		return c
	}

	var summary []string
	for _, pace := range []string{"instant", ""} {
		runs := envInt("MEASURE_RUNS", 30)
		if pace == "" {
			runs = envInt("MEASURE_HUMAN_RUNS", 10)
		}
		for _, name := range []string{"REST", "MCP"} {
			v := vias[name]
			for _, s := range []int{0, 200} {
				slow = s
				reset()
				lost, chars := 0, 0
				var took []time.Duration
				for range runs {
					n, next := cur, nextID()
					to := ks.Arrived("a"+n, ks.Keys(next, slow, "field"))
					start := time.Now()
					v.key("ctrl+l", pace)
					v.typ(to, pace)
					v.key("Return", pace)
					took = append(took, time.Since(start))
					_, arrived := ks.WaitRun("a"+n, 2500*time.Millisecond, func(r browsertest.KeyRun) bool { return r.Arrived })
					if arrived {
						if _, ok := ks.WaitRun(next, 15*time.Second, func(r browsertest.KeyRun) bool { return r.Ready }); ok {
							cur = next
							continue
						}
					}
					quiet(n)
					if !arrived {
						lost++
						chars += seen(n)
					}
					reset()
				}
				slices.Sort(took)
				p := pace
				if p == "" {
					p = "human (default)"
				}
				line := fmt.Sprintf("%s, %s pace, slow=%d: lost %d of %d", name, p, slow, lost, runs)
				if lost > 0 {
					line += fmt.Sprintf(" (the page saw %.1f characters a lost run)", float64(chars)/float64(lost))
				}
				line += fmt.Sprintf("; three calls took %v median", took[len(took)/2].Round(time.Millisecond))
				t.Log(line)
				summary = append(summary, line)
			}
		}
	}
	for _, s := range summary {
		t.Log(s)
	}
}

func envInt(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(name)); err == nil {
		return n
	}
	return def
}
