package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/moby/moby/client"
)

// fakeDaemon answers the two calls Stop makes, the way the Docker API does.
type fakeDaemon struct {
	stopStatus    int    // the stop call's status; 0 is 204
	state         string // what an inspect reports: "running", "exited"; "" is 404
	inspectStatus int    // fails the inspect with this status instead
	inspects      int
}

var apiVersion = regexp.MustCompile(`^/v[0-9.]+`)

func (d *fakeDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := apiVersion.ReplaceAllString(r.URL.Path, "")
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": msg})
	}
	switch {
	case path == "/_ping":
		w.Header().Set("Api-Version", "1.44")
		_, _ = w.Write([]byte("OK"))
	case r.Method == http.MethodPost && path == "/containers/c1/stop":
		switch d.stopStatus {
		case 0:
		case http.StatusNotFound:
			fail(d.stopStatus, "no such container: c1")
			return
		default:
			fail(d.stopStatus, "rootless netns: kill network process: permission denied")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case r.Method == http.MethodGet && path == "/containers/c1/json":
		d.inspects++
		if d.inspectStatus != 0 {
			fail(d.inspectStatus, "the daemon is unwell")
			return
		}
		if d.state == "" {
			fail(http.StatusNotFound, "no such container: c1")
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Id":    "c1",
			"State": map[string]any{"Status": d.state, "Running": d.state == "running"},
		})
	default:
		fail(http.StatusNotImplemented, r.Method+" "+path)
	}
}

func fakeClient(t *testing.T, d *fakeDaemon) (*Client, *bytes.Buffer) {
	t.Helper()
	srv := httptest.NewServer(d)
	t.Cleanup(srv.Close)
	api, err := client.New(client.WithHost("tcp://"+srv.Listener.Addr().String()), client.WithAPIVersion("1.44"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = api.Close() })
	var logs bytes.Buffer
	return &Client{api: api, log: slog.New(slog.NewTextHandler(&logs, nil))}, &logs
}

// PROJ-64: rootless Podman on Ubuntu answers a stop with 500 when AppArmor
// keeps it from killing pasta, though the container has stopped.
func TestStopToleratesAnErrorWhenTheContainerStopped(t *testing.T) {
	d := &fakeDaemon{stopStatus: http.StatusInternalServerError, state: "exited"}
	c, logs := fakeClient(t, d)
	if err := c.Stop(context.Background(), "c1"); err != nil {
		t.Fatalf("Stop = %v, want nil: the container did stop", err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "kill network process") {
		t.Fatalf("the daemon's error wasn't logged as a warning:\n%s", logs)
	}
}

func TestStopFailsWhenTheContainerIsStillRunning(t *testing.T) {
	d := &fakeDaemon{stopStatus: http.StatusInternalServerError, state: "running"}
	c, _ := fakeClient(t, d)
	if err := c.Stop(context.Background(), "c1"); err == nil || !strings.Contains(err.Error(), "kill network process") {
		t.Fatalf("Stop = %v, want the daemon's error", err)
	}
}

// The session's watcher can see the container exit and remove it before
// the stop's error comes back, so the inspect finds no container.
func TestStopToleratesAnErrorWhenTheContainerIsAlreadyRemoved(t *testing.T) {
	d := &fakeDaemon{stopStatus: http.StatusInternalServerError}
	c, logs := fakeClient(t, d)
	if err := c.Stop(context.Background(), "c1"); err != nil {
		t.Fatalf("Stop = %v, want nil: the container stopped and was removed", err)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "state=removed") {
		t.Fatalf("the daemon's error wasn't logged as a warning:\n%s", logs)
	}
}

func TestStopFailsWhenTheContainerCantBeInspected(t *testing.T) {
	d := &fakeDaemon{stopStatus: http.StatusInternalServerError, inspectStatus: http.StatusInternalServerError}
	c, _ := fakeClient(t, d)
	if err := c.Stop(context.Background(), "c1"); err == nil || !strings.Contains(err.Error(), "kill network process") {
		t.Fatalf("Stop = %v, want the stop's error, not the inspect's", err)
	}
}

func TestStopOfAMissingContainerFails(t *testing.T) {
	d := &fakeDaemon{stopStatus: http.StatusNotFound}
	c, _ := fakeClient(t, d)
	if err := c.Stop(context.Background(), "c1"); err == nil {
		t.Fatal("Stop of a container that doesn't exist = nil")
	}
	if d.inspects != 0 {
		t.Fatal("Stop inspected a container the stop didn't find")
	}
}

func TestStopDoesntInspectAfterACleanStop(t *testing.T) {
	d := &fakeDaemon{state: "exited"}
	c, logs := fakeClient(t, d)
	if err := c.Stop(context.Background(), "c1"); err != nil {
		t.Fatal(err)
	}
	if d.inspects != 0 || logs.Len() != 0 {
		t.Fatalf("%d inspects, logs %q after a clean stop", d.inspects, logs)
	}
}

func TestStopReturnsTheContextsError(t *testing.T) {
	d := &fakeDaemon{state: "exited"}
	c, _ := fakeClient(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Stop(ctx, "c1"); err == nil {
		t.Fatal("Stop with a cancelled context = nil")
	}
	if d.inspects != 0 {
		t.Fatal("Stop inspected the container after its context ended")
	}
}
