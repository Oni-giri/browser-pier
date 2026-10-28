// Package docker wraps the Docker SDK: it starts and stops browser
// containers, only ever from BrowserPier's own browser image.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/system"
	"github.com/moby/moby/client"

	"github.com/Oni-giri/browser-pier/images/chromium"
)

// Labels on every browser container.
const (
	LabelManaged = "io.browserpier.managed"
	LabelProfile = "io.browserpier.profile"
	LabelSession = "io.browserpier.session"
)

const (
	// StopTimeout is how long a browser container gets to stop before Docker
	// kills it. The entrypoint gives Chromium 20 s of it to close.
	StopTimeout = 30 * time.Second
	// shmSize is /dev/shm in the container; the image falls back to slower
	// shared memory below 512 MiB.
	shmSize = 1 << 30
)

// BrowserSpec is what varies between browser containers. Everything else,
// the image included, is fixed by Client.
type BrowserSpec struct {
	Name    string // container name
	Profile string
	Session string
	// ProfileDir and ControlDir are host paths, as the Docker daemon sees them.
	ProfileDir string // Chromium's user data dir
	ControlDir string // holds the control socket; shared with no other container
	User       string // uid:gid to run as
	Screen     string // WIDTHxHEIGHT
	// MountCheck is the value the orchestrator wrote to the marker file in
	// both folders; the entrypoint refuses to start if it doesn't find it.
	MountCheck string
}

// Container is a browser container as Docker lists it.
type Container struct {
	ID      string
	Name    string
	Profile string
	Session string
	State   string // Docker's state: created, running, exited, ...
}

// Client starts browser containers from one image.
type Client struct {
	api     *client.Client
	image   string
	seccomp string // the profile JSON, compact
	// seccompOpt is the security option that applies it: the JSON itself
	// for Docker, a host path for Podman (see UseDataDir).
	seccompOpt string
	userns     string
}

// Options configure a Client.
type Options struct {
	Image string // the browser image every container starts from
	// UsernsMode is the containers' user namespace mode. Rootless Podman
	// needs "keep-id" so the browser's uid matches the orchestrator's on
	// the profile folders; Docker needs nothing.
	UsernsMode string
}

// New connects to the Docker daemon named by the usual DOCKER_* variables.
func New(o Options) (*Client, error) {
	image := o.Image
	if image == "" {
		return nil, errors.New("no browser image set")
	}
	var seccomp bytes.Buffer
	if err := json.Compact(&seccomp, chromium.SeccompProfile); err != nil {
		return nil, fmt.Errorf("embedded seccomp profile: %w", err)
	}
	api, err := client.New(client.FromEnv)
	if err != nil {
		return nil, err
	}
	return &Client{api: api, image: image, seccomp: seccomp.String(), seccompOpt: "seccomp=" + seccomp.String(), userns: o.UsernsMode}, nil
}

// UseDataDir finishes setting up against the daemon, given the data dir as
// this process and as the daemon see it. Podman's Docker API takes the
// seccomp profile as a path rather than inline, so for Podman the profile
// is written into the data dir and passed by its host path.
func (c *Client) UseDataDir(ctx context.Context, dataDir, hostDataDir string) error {
	v, err := c.api.ServerVersion(ctx, client.ServerVersionOptions{})
	if err != nil {
		return err
	}
	if !isPodman(v.Components) {
		return nil
	}
	root, err := os.OpenRoot(dataDir)
	if err != nil {
		return err
	}
	defer func() { _ = root.Close() }()
	if err := root.WriteFile("seccomp.json.tmp", []byte(c.seccomp), 0o644); err != nil {
		return err
	}
	if err := root.Rename("seccomp.json.tmp", "seccomp.json"); err != nil {
		return err
	}
	c.seccompOpt = "seccomp=" + filepath.Join(hostDataDir, "seccomp.json")
	return nil
}

func isPodman(components []system.ComponentVersion) bool {
	for _, comp := range components {
		if strings.Contains(comp.Name, "Podman") {
			return true
		}
	}
	return false
}

func (c *Client) Close() error { return c.api.Close() }

// Image is the browser image every container starts from.
func (c *Client) Image() string { return c.image }

// Ping checks the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.api.Ping(ctx, client.PingOptions{})
	return err
}

// Create creates a browser container, pulling the image first if Docker
// doesn't have it.
func (c *Client) Create(ctx context.Context, spec BrowserSpec) (string, error) {
	cfg, host := browserConfig(c.image, c.seccompOpt, spec)
	host.UsernsMode = container.UsernsMode(c.userns)
	opts := client.ContainerCreateOptions{Name: spec.Name, Config: cfg, HostConfig: host}
	res, err := c.api.ContainerCreate(ctx, opts)
	if cerrdefs.IsNotFound(err) {
		if err := c.pull(ctx); err != nil {
			return "", err
		}
		res, err = c.api.ContainerCreate(ctx, opts)
	}
	if err != nil {
		return "", err
	}
	return res.ID, nil
}

func (c *Client) pull(ctx context.Context) error {
	resp, err := c.api.ImagePull(ctx, c.image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pulling %s: %w", c.image, err)
	}
	defer func() { _ = resp.Close() }()
	if err := resp.Wait(ctx); err != nil {
		return fmt.Errorf("pulling %s: %w", c.image, err)
	}
	return nil
}

// browserConfig is the whole container definition. It publishes no ports.
func browserConfig(image, seccompOpt string, spec BrowserSpec) (*container.Config, *container.HostConfig) {
	stop := int(StopTimeout / time.Second)
	initProc := false // the image's entrypoint is PID 1 and reaps
	cfg := &container.Config{
		Image: image,
		User:  spec.User,
		Env: []string{
			"BROWSERPIER_SCREEN=" + spec.Screen,
			"BROWSERPIER_MOUNT_CHECK=" + spec.MountCheck,
		},
		Labels: map[string]string{
			LabelManaged: "true",
			LabelProfile: spec.Profile,
			LabelSession: spec.Session,
		},
		StopTimeout: &stop,
	}
	host := &container.HostConfig{
		Mounts: []mount.Mount{
			{Type: mount.TypeBind, Source: spec.ProfileDir, Target: "/profile"},
			{Type: mount.TypeBind, Source: spec.ControlDir, Target: "/run/browserpier"},
		},
		// Chromium's sandbox needs the seccomp profile (see images/chromium)
		// and no setuid. The browser's user has no effective capabilities,
		// but the sandbox chroots inside its own user namespace, which needs
		// SYS_CHROOT left in the bounding set; nothing else is.
		SecurityOpt: []string{seccompOpt, "no-new-privileges:true"},
		CapDrop:     []string{"ALL"},
		CapAdd:      []string{"SYS_CHROOT"},
		ShmSize:     shmSize,
		Init:        &initProc,
	}
	return cfg, host
}

func (c *Client) Start(ctx context.Context, id string) error {
	_, err := c.api.ContainerStart(ctx, id, client.ContainerStartOptions{})
	return err
}

// Stop asks the container to stop and returns once it has, killing it after
// StopTimeout.
func (c *Client) Stop(ctx context.Context, id string) error {
	_, err := c.api.ContainerStop(ctx, id, client.ContainerStopOptions{})
	return err
}

// Wait blocks until the container isn't running and returns its exit code.
func (c *Client) Wait(ctx context.Context, id string) (int, error) {
	res := c.api.ContainerWait(ctx, id, client.ContainerWaitOptions{Condition: container.WaitConditionNotRunning})
	select {
	case r := <-res.Result:
		if r.Error != nil && r.Error.Message != "" {
			return -1, errors.New(r.Error.Message)
		}
		return int(r.StatusCode), nil
	case err := <-res.Error:
		return -1, err
	}
}

// LastLogLine returns the container's last non-empty log line.
func (c *Client) LastLogLine(ctx context.Context, id string) (string, error) {
	r, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true, ShowStderr: true, Tail: "50"})
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	var buf bytes.Buffer
	if _, err := stdcopy.StdCopy(&buf, &buf, r); err != nil {
		return "", err
	}
	return lastLine(buf.Bytes()), nil
}

const maxLogLine = 500

func lastLine(b []byte) string {
	var last string
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		if l := strings.TrimSpace(sc.Text()); l != "" {
			last = l
		}
	}
	if len(last) > maxLogLine {
		last = strings.ToValidUTF8(last[:maxLogLine], "") + "…"
	}
	return last
}

// Remove removes a stopped container, by ID or name. A missing one is fine.
func (c *Client) Remove(ctx context.Context, id string) error {
	_, err := c.api.ContainerRemove(ctx, id, client.ContainerRemoveOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveStale removes a leftover container that holds name, but only a
// stopped one BrowserPier made for this profile. Any other container by
// that name is left alone and reported, never deleted.
func (c *Client) RemoveStale(ctx context.Context, name, profile string) error {
	res, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var labels map[string]string
	if res.Container.Config != nil {
		labels = res.Container.Config.Labels
	}
	if labels[LabelManaged] != "true" || labels[LabelProfile] != profile {
		return fmt.Errorf("another container is already named %s; BrowserPier didn't create it, so rename or remove it yourself", name)
	}
	if res.Container.State != nil && res.Container.State.Running {
		return fmt.Errorf("an earlier browser container for this profile (%s) is still running", name)
	}
	return c.Remove(ctx, res.Container.ID)
}

// List returns every browser container BrowserPier started, running or not.
func (c *Client) List(ctx context.Context) ([]Container, error) {
	res, err := c.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true,
		Filters: make(client.Filters).Add("label", LabelManaged+"=true"),
	})
	if err != nil {
		return nil, err
	}
	out := make([]Container, 0, len(res.Items))
	for _, it := range res.Items {
		name := ""
		if len(it.Names) > 0 {
			name = strings.TrimPrefix(it.Names[0], "/")
		}
		out = append(out, Container{
			ID:      it.ID,
			Name:    name,
			Profile: it.Labels[LabelProfile],
			Session: it.Labels[LabelSession],
			State:   string(it.State),
		})
	}
	return out, nil
}

// User formats a uid and gid the way Docker takes them.
func User(uid, gid int) string { return strconv.Itoa(uid) + ":" + strconv.Itoa(gid) }
