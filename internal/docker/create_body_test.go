package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/moby/moby/client"
)

// updateGolden rewrites testdata/create-*.json from what the code sends now.
// The files were first written on origin/main before the create guard
// (PROJ-159), so a change to them is a change to the containers.
var updateGolden = flag.Bool("update-golden", false, "rewrite the golden create bodies")

// createDaemon answers what Create and the sandbox check send, recording
// every create and pull.
type createDaemon struct {
	mu       sync.Mutex
	missing  bool // the first create finds no image, as before a pull
	creates  []recordedCreate
	pulls    int
	requests []string
}

type recordedCreate struct {
	name string
	body []byte
}

func (d *createDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := apiVersion.ReplaceAllString(r.URL.Path, "")
	d.mu.Lock()
	defer d.mu.Unlock()
	d.requests = append(d.requests, r.Method+" "+path)
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.44")
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodPost && path == "/containers/create":
		body, _ := io.ReadAll(r.Body)
		d.creates = append(d.creates, recordedCreate{name: r.URL.Query().Get("name"), body: body})
		if d.missing {
			d.missing = false
			fail(http.StatusNotFound, "No such image: img")
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"Id": "new1", "Warnings": []string{}})
	case r.Method == http.MethodPost && path == "/images/create":
		d.pulls++
		_, _ = w.Write([]byte(`{"status":"done"}` + "\n"))
	case r.Method == http.MethodGet && path == "/containers/browserpier_check_i1/json":
		fail(http.StatusNotFound, "no such container")
	default:
		// The check's start and remove: the check ends there, unknown.
		fail(http.StatusNotImplemented, r.Method+" "+path)
	}
}

func (d *createDaemon) counts() (creates, pulls int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return len(d.creates), d.pulls
}

const goldenImage = "ghcr.io/oni-giri/browser-pier-chromium:1.0.0"

// goldenClient is a Client as UseDataDir leaves it, by hand, talking to d.
// podman picks Podman's form of the seccomp option and keep-id.
func goldenClient(t *testing.T, d http.Handler, podman bool, apparmorOpt string) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	api, err := client.New(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	seccomp := compactSeccomp(t)
	c := &Client{api: api, image: goldenImage, seccomp: seccomp, seccompOpt: "seccomp=" + seccomp,
		user: "1000:1000", instance: "i1",
		plan:        appArmorPlan{try: []string{optBrowserPier, "", optUnconfined}, fallback: optUnconfined},
		apparmorOpt: apparmorOpt, apparmorSettled: true}
	setHostDataDir(c, "/srv/bp")
	if podman {
		c.seccompOpt = "seccomp=" + filepath.Join("/srv/bp", "seccomp.json")
		c.userns = "keep-id"
		c.plan = appArmorPlan{}
	}
	var logs bytes.Buffer
	c.log = slog.New(slog.NewTextHandler(&logs, nil))
	return c, &logs
}

func compactSeccomp(t *testing.T) string {
	t.Helper()
	c, err := New(Options{Image: "img", Instance: "i1"})
	if err != nil {
		t.Fatal(err)
	}
	_ = c.Close()
	return c.seccomp
}

// goldenSpec is what the session manager asks for, as on a real start.
func goldenSpec() BrowserSpec {
	return BrowserSpec{
		Name: "browserpier-google", Profile: "google", Session: "01JD3XKQ8V5W6Y7Z8A9B0C1D2E",
		ProfileDir: "/srv/bp/profiles/google/chrome", ControlDir: "/srv/bp/profiles/google/run", FilesDir: "/srv/bp/profiles/google/files",
		User: "1000:1000", Screen: "1440x900", MountCheck: "01JD3XKQ8V5W6Y7Z8A9B0C1D2E",
		Env: []string{
			"BROWSERPIER_GEOIP_URL=https://ipinfo.io/json",
			"BROWSERPIER_GEO_COUNTRY=FR",
			"BROWSERPIER_GEO_TZ=Europe/Paris",
			"BROWSERPIER_TZ_OVERRIDE=",
			"BROWSERPIER_LOCALE_OVERRIDE=fr-FR",
			"BROWSERPIER_LOCAL_NETWORK=false",
			"BROWSERPIER_HOST_IPS=192.168.1.10",
			"BROWSERPIER_BLOCK_ADS=true",
		},
	}
}

// checkGolden compares a create body with testdata/name, the inline
// seccomp profile replaced by a placeholder so the file doesn't follow
// every change to the profile.
func checkGolden(t *testing.T, name string, got recordedCreate) {
	t.Helper()
	esc, _ := json.Marshal("seccomp=" + compactSeccomp(t))
	body := bytes.ReplaceAll(got.body, esc[1:len(esc)-1], []byte("seccomp=<the embedded profile>"))
	body = append([]byte("name="+got.name+"\n"), body...)
	file := filepath.Join("testdata", name)
	if *updateGolden {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(file, body, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(body, want) {
		t.Fatalf("the create body changed from %s:\n got %s\nwant %s", file, body, want)
	}
}

// The bodies sent to the daemon are what they were before the guard: the
// guard checks them, it doesn't change them.
func TestCreateBodiesAreUnchanged(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		golden      string
		podman      bool
		apparmorOpt string
	}{
		{"create-browser-docker.json", false, optBrowserPier},
		{"create-browser-docker-default.json", false, ""},
		{"create-browser-podman.json", true, ""},
	} {
		d := &createDaemon{missing: true}
		cl, _ := goldenClient(t, d, c.podman, c.apparmorOpt)
		if _, err := cl.Create(ctx, goldenSpec()); err != nil {
			t.Fatalf("%s: %v", c.golden, err)
		}
		if creates, pulls := d.counts(); creates != 2 || pulls != 1 {
			t.Fatalf("%s: %d creates, %d pulls; want the create, a pull and the create again", c.golden, creates, pulls)
		}
		if !bytes.Equal(d.creates[0].body, d.creates[1].body) || d.creates[0].name != d.creates[1].name {
			t.Fatalf("%s: the create after the pull sent another body", c.golden)
		}
		checkGolden(t, c.golden, d.creates[0])
	}

	d := &createDaemon{}
	cl, _ := goldenClient(t, d, false, "")
	if v, _ := cl.checkSandbox(ctx, optUnconfined); v != unknown {
		t.Fatalf("the fake daemon's check: %v", v)
	}
	if creates, _ := d.counts(); creates != 1 {
		t.Fatalf("%d creates for one check", creates)
	}
	checkGolden(t, "create-sandbox-check.json", d.creates[0])
}
