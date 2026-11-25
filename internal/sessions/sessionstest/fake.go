// Package sessionstest has a fake container runtime for tests: started
// Containers serve a control socket, the way the browser image does.
package sessionstest

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

// FakeRuntime stands in for Docker. A started container serves /healthz on
// the control socket the way the browser image does.
type FakeRuntime struct {
	Mu         sync.Mutex
	dataDir    string
	Containers map[string]*FakeContainer
	next       int
	Creates    int
	Stops      int

	// Knobs, read when a container starts or Stops.
	ExitOnStart *int // exit at once with this status
	Unhealthy   bool // never answer on the socket
	StopStatus  int
	StopErr     error   // Stop fails with this, leaving the container running
	StartErrs   []error // each Start fails with the next of these, while any are left
	// BeforeStop, if set, is called (without the lock) as Stop begins.
	BeforeStop func(id string)
	// AfterStop, if set, is called (without the lock) once Stop has made
	// the container exit, and Stop returns what it returns: a runtime that
	// reports an error after the container has stopped (PROJ-64).
	AfterStop func(id string) error
	LastLog   string
	// Memory is what MemoryUsage reports for every container.
	Memory int64
	// Control, if set, serves each started container's control socket
	// instead of the default, which answers /healthz and empties the
	// clipboard only.
	Control http.Handler
}

type FakeContainer struct {
	ID, State string
	Foreign   bool // not created by BrowserPier
	Spec      docker.BrowserSpec
	Version   string // the definition's version, docker.Spec when created here
	Exited    chan struct{}
	Status    int
	srv       *http.Server
}

func New(dataDir string) *FakeRuntime {
	return &FakeRuntime{dataDir: dataDir, Containers: map[string]*FakeContainer{}}
}

func (f *FakeRuntime) Create(_ context.Context, spec docker.BrowserSpec) (string, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for _, c := range f.Containers {
		if c.Spec.Name == spec.Name {
			return "", fmt.Errorf("name %s in use", spec.Name)
		}
	}
	f.next++
	f.Creates++
	c := &FakeContainer{ID: fmt.Sprintf("c%d", f.next), State: "created", Spec: spec, Version: docker.Spec, Exited: make(chan struct{})}
	f.Containers[c.ID] = c
	return c.ID, nil
}

func (f *FakeRuntime) Start(_ context.Context, id string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return errors.New("no such container")
	}
	if len(f.StartErrs) > 0 {
		err := f.StartErrs[0]
		f.StartErrs = f.StartErrs[1:]
		return err
	}
	c.State = "running"
	if f.ExitOnStart != nil {
		f.exitLocked(c, *f.ExitOnStart)
		return nil
	}
	if !f.Unhealthy {
		// The fake runs on this host, so the spec's host path is ours too.
		ln, err := net.Listen("unix", filepath.Join(c.Spec.ControlDir, "control.sock"))
		if err != nil {
			return err
		}
		h := f.Control
		if h == nil {
			mux := http.NewServeMux()
			mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
			// A person's control ending empties the clipboard (PROJ-194).
			mux.HandleFunc("DELETE /input/clipboard", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			// And lets go of what the person held.
			mux.HandleFunc("POST /input/release", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
			h = mux
		}
		c.srv = &http.Server{Handler: h} //nolint:gosec // test server
		go func() { _ = c.srv.Serve(ln) }()
	}
	return nil
}

// Exit makes a running container exit with status.
func (f *FakeRuntime) Exit(id string, status int) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	f.exitLocked(f.Containers[id], status)
}

func (f *FakeRuntime) exitLocked(c *FakeContainer, status int) {
	if c.State != "running" {
		return
	}
	if c.srv != nil {
		_ = c.srv.Close()
	}
	c.State, c.Status = "exited", status
	close(c.Exited)
}

func (f *FakeRuntime) Stop(_ context.Context, id string) error {
	f.Mu.Lock()
	before := f.BeforeStop
	f.Mu.Unlock()
	if before != nil {
		before(id)
	}
	f.Mu.Lock()
	c, ok := f.Containers[id]
	if !ok {
		f.Mu.Unlock()
		return errors.New("no such container")
	}
	f.Stops++
	if f.StopErr != nil {
		f.Mu.Unlock()
		return f.StopErr
	}
	f.exitLocked(c, f.StopStatus)
	after := f.AfterStop
	f.Mu.Unlock()
	if after != nil {
		return after(id)
	}
	return nil
}

func (f *FakeRuntime) Wait(ctx context.Context, id string) (int, error) {
	f.Mu.Lock()
	c, ok := f.Containers[id]
	f.Mu.Unlock()
	if !ok {
		return -1, errors.New("no such container")
	}
	select {
	case <-c.Exited:
		return c.Status, nil
	case <-ctx.Done():
		return -1, ctx.Err()
	}
}

func (f *FakeRuntime) LastLogLine(context.Context, string) (string, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return f.LastLog, nil
}

func (f *FakeRuntime) Remove(_ context.Context, idOrName string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for id, c := range f.Containers {
		if id == idOrName || c.Spec.Name == idOrName {
			if c.State == "running" {
				return errors.New("container is running")
			}
			delete(f.Containers, id)
		}
	}
	return nil
}

func (f *FakeRuntime) RemoveStale(_ context.Context, name, profile string) error {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	for id, c := range f.Containers {
		if c.Spec.Name != name {
			continue
		}
		if c.Foreign || c.Spec.Profile != profile {
			return fmt.Errorf("another container is already named %s", name)
		}
		if c.State == "running" {
			return errors.New("still running")
		}
		delete(f.Containers, id)
	}
	return nil
}

func (f *FakeRuntime) MemoryUsage(_ context.Context, id string) (int64, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	if _, ok := f.Containers[id]; !ok {
		return 0, errors.New("no such container")
	}
	return f.Memory, nil
}

func (f *FakeRuntime) List(context.Context) ([]docker.Container, error) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	var out []docker.Container
	for _, c := range f.Containers {
		if c.Foreign {
			continue // List filters on BrowserPier's label
		}
		out = append(out, docker.Container{ID: c.ID, Name: c.Spec.Name, Profile: c.Spec.Profile, Session: c.Spec.Session, State: c.State, Spec: c.Version})
	}
	return out, nil
}

func (f *FakeRuntime) Container(id string) (FakeContainer, bool) {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	c, ok := f.Containers[id]
	if !ok {
		return FakeContainer{}, false
	}
	return *c, true
}

func (f *FakeRuntime) Count() int {
	f.Mu.Lock()
	defer f.Mu.Unlock()
	return len(f.Containers)
}

// Outdated reports a container created under another definition than
// docker.Spec, which Create labels containers with.
func (f *FakeRuntime) Outdated(spec string) bool { return spec != docker.Spec }
