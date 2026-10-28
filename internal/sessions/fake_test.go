package sessions

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"

	"github.com/Oni-giri/browser-pier/internal/docker"
)

// fakeRuntime stands in for Docker. A started container serves /healthz on
// the control socket the way the browser image does.
type fakeRuntime struct {
	mu         sync.Mutex
	dataDir    string
	containers map[string]*fakeContainer
	next       int
	creates    int
	stops      int

	// Knobs, read when a container starts or stops.
	exitOnStart *int // exit at once with this status
	unhealthy   bool // never answer on the socket
	stopStatus  int
	stopErr     error // Stop fails with this, leaving the container running
	lastLog     string
}

type fakeContainer struct {
	id, state string
	foreign   bool // not created by BrowserPier
	spec      docker.BrowserSpec
	exited    chan struct{}
	status    int
	srv       *http.Server
}

func newFakeRuntime(dataDir string) *fakeRuntime {
	return &fakeRuntime{dataDir: dataDir, containers: map[string]*fakeContainer{}}
}

func (f *fakeRuntime) Create(_ context.Context, spec docker.BrowserSpec) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.containers {
		if c.spec.Name == spec.Name {
			return "", fmt.Errorf("name %s in use", spec.Name)
		}
	}
	f.next++
	f.creates++
	c := &fakeContainer{id: fmt.Sprintf("c%d", f.next), state: "created", spec: spec, exited: make(chan struct{})}
	f.containers[c.id] = c
	return c.id, nil
}

func (f *fakeRuntime) Start(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return errors.New("no such container")
	}
	c.state = "running"
	if f.exitOnStart != nil {
		f.exitLocked(c, *f.exitOnStart)
		return nil
	}
	if !f.unhealthy {
		// The fake runs on this host, so the spec's host path is ours too.
		ln, err := net.Listen("unix", filepath.Join(c.spec.ControlDir, "control.sock"))
		if err != nil {
			return err
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
		c.srv = &http.Server{Handler: mux} //nolint:gosec // test server
		go func() { _ = c.srv.Serve(ln) }()
	}
	return nil
}

// exit makes a running container exit with status.
func (f *fakeRuntime) exit(id string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exitLocked(f.containers[id], status)
}

func (f *fakeRuntime) exitLocked(c *fakeContainer, status int) {
	if c.state != "running" {
		return
	}
	if c.srv != nil {
		_ = c.srv.Close()
	}
	c.state, c.status = "exited", status
	close(c.exited)
}

func (f *fakeRuntime) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return errors.New("no such container")
	}
	f.stops++
	if f.stopErr != nil {
		return f.stopErr
	}
	f.exitLocked(c, f.stopStatus)
	return nil
}

func (f *fakeRuntime) Wait(ctx context.Context, id string) (int, error) {
	f.mu.Lock()
	c, ok := f.containers[id]
	f.mu.Unlock()
	if !ok {
		return -1, errors.New("no such container")
	}
	select {
	case <-c.exited:
		return c.status, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (f *fakeRuntime) LastLogLine(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastLog, nil
}

func (f *fakeRuntime) Remove(_ context.Context, idOrName string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, c := range f.containers {
		if id == idOrName || c.spec.Name == idOrName {
			if c.state == "running" {
				return errors.New("container is running")
			}
			delete(f.containers, id)
		}
	}
	return nil
}

func (f *fakeRuntime) RemoveStale(_ context.Context, name, profile string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, c := range f.containers {
		if c.spec.Name != name {
			continue
		}
		if c.foreign || c.spec.Profile != profile {
			return fmt.Errorf("another container is already named %s", name)
		}
		if c.state == "running" {
			return errors.New("still running")
		}
		delete(f.containers, id)
	}
	return nil
}

func (f *fakeRuntime) List(context.Context) ([]docker.Container, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []docker.Container
	for _, c := range f.containers {
		if c.foreign {
			continue // List filters on BrowserPier's label
		}
		out = append(out, docker.Container{ID: c.id, Name: c.spec.Name, Profile: c.spec.Profile, Session: c.spec.Session, State: c.state})
	}
	return out, nil
}

func (f *fakeRuntime) container(id string) (fakeContainer, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	c, ok := f.containers[id]
	if !ok {
		return fakeContainer{}, false
	}
	return *c, true
}

func (f *fakeRuntime) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.containers)
}
