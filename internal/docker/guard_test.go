package docker

import (
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"log/slog"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
)

// The plans planAppArmor can give, and the option each leads browsers to.
var guardPlans = []struct {
	name string
	plan appArmorPlan
	opts []string // the options a browser or a check may get under it
}{
	{"none", appArmorPlan{}, []string{""}},
	{"auto", appArmorPlan{try: []string{optBrowserPier, "", optUnconfined}, fallback: optUnconfined}, []string{optBrowserPier, "", optUnconfined}},
	{"remote", appArmorPlan{try: []string{optBrowserPier, "", optUnconfined}}, []string{optBrowserPier, "", optUnconfined}},
	{"admin", appArmorPlan{fixed: "apparmor=my-browsers"}, []string{"apparmor=my-browsers"}},
}

// guardClient is a Client as UseDataDir leaves it, without a daemon.
func guardClient(t *testing.T, podman bool, plan appArmorPlan) *Client {
	t.Helper()
	seccomp := compactSeccomp(t)
	c := &Client{image: goldenImage, seccomp: seccomp, seccompOpt: "seccomp=" + seccomp, user: "1000:1000", instance: "i1",
		plan: plan, hostDataDir: "/srv/bp", log: slog.New(slog.DiscardHandler)}
	if podman {
		c.seccompOpt = "seccomp=/srv/bp/seccomp.json"
		c.userns = "keep-id"
	}
	return c
}

// goldenProxyTest is the proxy test's spec under c's host data dir, as
// sessions.Manager.TestProxy makes it.
func goldenProxyTest(c *Client) ProxyTestSpec {
	return ProxyTestSpec{Dir: filepath.Join(c.hostDataDir, "proxytest"), GeoURL: "https://ipinfo.io/json"}
}

// allKinds are the kinds of container BrowserPier creates.
var allKinds = []createKind{kindBrowser, kindSandboxCheck, kindProxyTest}

// bodyOf is a valid body of kind under the AppArmor option opt, made by
// the real builder.
func bodyOf(c *Client, kind createKind, opt string) client.ContainerCreateOptions {
	switch kind {
	case kindSandboxCheck:
		return c.sandboxCheckOptions(opt)
	case kindProxyTest:
		return c.proxyTestOptions(opt, goldenProxyTest(c))
	}
	return c.browserCreateOptions(opt, goldenSpec())
}

func TestGuardAcceptsWhatTheBuildersMake(t *testing.T) {
	for _, podman := range []bool{false, true} {
		for _, p := range guardPlans {
			c := guardClient(t, podman, p.plan)
			for _, opt := range p.opts {
				if err := c.checkCreate(kindBrowser, c.browserCreateOptions(opt, goldenSpec())); err != nil {
					t.Errorf("podman %t, plan %s, %q: browser refused: %v", podman, p.name, opt, err)
				}
				if err := c.checkCreate(kindSandboxCheck, c.sandboxCheckOptions(opt)); err != nil {
					t.Errorf("podman %t, plan %s, %q: check refused: %v", podman, p.name, opt, err)
				}
				if err := c.checkCreate(kindProxyTest, c.proxyTestOptions(opt, goldenProxyTest(c))); err != nil {
					t.Errorf("podman %t, plan %s, %q: proxy test refused: %v", podman, p.name, opt, err)
				}
			}
		}
	}
	// keep-id with options, as Podman takes them.
	c := guardClient(t, true, appArmorPlan{})
	c.userns = "keep-id:uid=1000,gid=1000"
	if err := c.checkCreate(kindBrowser, c.browserCreateOptions("", goldenSpec())); err != nil {
		t.Errorf("keep-id with options: %v", err)
	}
	// Each environment variable is optional but the first two, and a
	// value may be empty.
	spec := goldenSpec()
	spec.Env = nil
	c = guardClient(t, false, appArmorPlan{})
	if err := c.checkCreate(kindBrowser, c.browserCreateOptions("", spec)); err != nil {
		t.Errorf("without the optional environment: %v", err)
	}
	// A temp dir as data dir, as the Docker tests have it.
	dir := t.TempDir()
	c.hostDataDir = dir
	spec = goldenSpec()
	spec.ProfileDir, spec.ControlDir, spec.FilesDir = filepath.Join(dir, "profiles/google/chrome"), filepath.Join(dir, "profiles/google/run"), filepath.Join(dir, "profiles/google/files")
	if err := c.checkCreate(kindBrowser, c.browserCreateOptions("", spec)); err != nil {
		t.Errorf("under a temp dir: %v", err)
	}
	if err := c.checkCreate(kindProxyTest, c.proxyTestOptions("", goldenProxyTest(c))); err != nil {
		t.Errorf("the proxy test under a temp dir: %v", err)
	}
}

// The user namespace modes allowed: Podman's keep-id options by name, each
// once, numeric (item 3 of the Codex review). New and the guard agree.
var (
	goodUserns = []string{"", "keep-id", "keep-id:uid=1000", "keep-id:uid=1000,gid=1000", "keep-id:size=65536",
		"keep-id:gid=0,uid=4294967295,size=1"}
	badUserns = []string{"host", "private", "auto", "nomap", "keep-id:", "keep-id:typo=1000", "keep-id:uid=1000,typo=1",
		"keep-id:uid=1,uid=2", "keep-id:uid=", "keep-id:uid=x", "keep-id:uid=+1", "keep-id:uid=4294967296",
		"keep-id:uid=1000,", "keep-id:,uid=1000", "keep-id:UID=1000", "keep-id;host", " keep-id", "Keep-id"}
)

func TestUsernsModes(t *testing.T) {
	for _, v := range goodUserns {
		if !ValidUsernsMode(v) {
			t.Errorf("%q refused", v)
		}
		c := guardClient(t, true, appArmorPlan{})
		c.userns = v
		if err := c.checkCreate(kindBrowser, c.browserCreateOptions("", goldenSpec())); err != nil {
			t.Errorf("%q: the guard refused it: %v", v, err)
		}
	}
	for _, v := range badUserns {
		if ValidUsernsMode(v) {
			t.Errorf("%q accepted", v)
		}
		c := guardClient(t, true, appArmorPlan{})
		c.userns = v
		err := c.checkCreate(kindBrowser, c.browserCreateOptions("", goldenSpec()))
		if !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "HostConfig.UsernsMode is neither unset nor keep-id") {
			t.Errorf("%q: the guard took it: %v", v, err)
		}
		if _, err := New(Options{Image: "img", Instance: "i1", User: "1000:1000", UsernsMode: v}); err == nil {
			t.Errorf("New took the user namespace mode %q", v)
		}
	}
}

type widening struct {
	name   string
	kind   createKind // the browser, unless set
	plan   *appArmorPlan
	opt    string // the AppArmor option the body is built with
	mutate func(o *client.ContainerCreateOptions)
	field  string // the field the error must name
}

func cfgOf(f func(*container.Config)) func(*client.ContainerCreateOptions) {
	return func(o *client.ContainerCreateOptions) { f(o.Config) }
}

func hostOf(f func(*container.HostConfig)) func(*client.ContainerCreateOptions) {
	return func(o *client.ContainerCreateOptions) { f(o.HostConfig) }
}

func setEnv(o *client.ContainerCreateOptions, name, value string) {
	for i, e := range o.Config.Env {
		if strings.HasPrefix(e, name+"=") {
			o.Config.Env[i] = name + "=" + value
		}
	}
}

func replaceOpt(opts []string, prefix, with string) []string {
	out := []string{}
	for _, o := range opts {
		if strings.HasPrefix(o, prefix) {
			if with != "" {
				out = append(out, with)
			}
			continue
		}
		out = append(out, o)
	}
	return out
}

var (
	fixedCustom = appArmorPlan{fixed: "apparmor=my-browsers"}
	tr          = true
	other       = 31
)

func pidsOf(n int64) *int64 { return &n }

// proxyLimits gives h the proxy test's limits.
func proxyLimits(h *container.HostConfig) {
	h.Memory, h.MemorySwap, h.PidsLimit = ProxyTestMemory, ProxyTestMemory, pidsOf(ProxyTestPids)
}

// browserMounts are a browser's three folders, as the proxy test must
// never get them.
var browserMounts = []mount.Mount{
	{Type: mount.TypeBind, Source: "/srv/bp/profiles/google/chrome", Target: "/profile"},
	{Type: mount.TypeBind, Source: "/srv/bp/profiles/google/run", Target: "/run/browserpier"},
	{Type: mount.TypeBind, Source: "/srv/bp/profiles/google/files", Target: "/files"},
}

var widenings = []widening{
	{name: "another image", mutate: cfgOf(func(c *container.Config) { c.Image = "evil/chromium:latest" }), field: "Config.Image"},
	{name: "the Image shortcut", mutate: func(o *client.ContainerCreateOptions) { o.Image = goldenImage }, field: "ContainerCreateOptions.Image"},
	{name: "empty user", mutate: cfgOf(func(c *container.Config) { c.User = "" }), field: "Config.User"},
	{name: "root", mutate: cfgOf(func(c *container.Config) { c.User = "0:0" }), field: "Config.User"},
	{name: "root, padded", mutate: cfgOf(func(c *container.Config) { c.User = "00:1000" }), field: "Config.User"},
	{name: "a user name", mutate: cfgOf(func(c *container.Config) { c.User = "chrome:chrome" }), field: "Config.User"},
	{name: "another uid", mutate: cfgOf(func(c *container.Config) { c.User = "1001:1000" }), field: "Config.User"},
	{name: "Privileged", mutate: hostOf(func(h *container.HostConfig) { h.Privileged = true }), field: "HostConfig.Privileged"},
	{name: "CapAdd SYS_ADMIN", mutate: hostOf(func(h *container.HostConfig) { h.CapAdd = append(h.CapAdd, "SYS_ADMIN") }), field: "HostConfig.CapAdd"},
	{name: "CapAdd empty", mutate: hostOf(func(h *container.HostConfig) { h.CapAdd = nil }), field: "HostConfig.CapAdd"},
	{name: "CapDrop empty", mutate: hostOf(func(h *container.HostConfig) { h.CapDrop = []string{} }), field: "HostConfig.CapDrop"},
	{name: "no no-new-privileges", mutate: hostOf(func(h *container.HostConfig) {
		h.SecurityOpt = replaceOpt(h.SecurityOpt, "no-new-privileges", "")
	}), field: "HostConfig.SecurityOpt"},
	{name: "no-new-privileges false", mutate: hostOf(func(h *container.HostConfig) {
		h.SecurityOpt = replaceOpt(h.SecurityOpt, "no-new-privileges", "no-new-privileges:false")
	}), field: "HostConfig.SecurityOpt"},
	{name: "seccomp=unconfined", mutate: hostOf(func(h *container.HostConfig) {
		h.SecurityOpt = replaceOpt(h.SecurityOpt, "seccomp=", "seccomp=unconfined")
	}), field: "HostConfig.SecurityOpt"},
	{name: "no seccomp", mutate: hostOf(func(h *container.HostConfig) {
		h.SecurityOpt = replaceOpt(h.SecurityOpt, "seccomp=", "")
	}), field: "HostConfig.SecurityOpt"},
	{name: "label=disable", mutate: hostOf(func(h *container.HostConfig) { h.SecurityOpt = append(h.SecurityOpt, "label=disable") }), field: "HostConfig.SecurityOpt"},
	{name: "systempaths=unconfined", mutate: hostOf(func(h *container.HostConfig) { h.SecurityOpt = append(h.SecurityOpt, "systempaths=unconfined") }), field: "HostConfig.SecurityOpt"},
	{name: "no-new-privileges twice", mutate: hostOf(func(h *container.HostConfig) { h.SecurityOpt = append(h.SecurityOpt, "no-new-privileges:true") }), field: "HostConfig.SecurityOpt"},
	{name: "unconfined when the admin named a profile", plan: &fixedCustom, opt: "apparmor=my-browsers", mutate: func(o *client.ContainerCreateOptions) {
		o.HostConfig.SecurityOpt = replaceOpt(o.HostConfig.SecurityOpt, "apparmor=", optUnconfined)
		o.Config.Labels[LabelSpec] = Spec + ";" + optUnconfined
	}, field: "HostConfig.SecurityOpt"},
	{name: "no AppArmor when the admin named a profile", plan: &fixedCustom, opt: "apparmor=my-browsers", mutate: func(o *client.ContainerCreateOptions) {
		o.HostConfig.SecurityOpt = replaceOpt(o.HostConfig.SecurityOpt, "apparmor=", "")
		o.Config.Labels[LabelSpec] = Spec
	}, field: "HostConfig.SecurityOpt"},
	{name: "an AppArmor option outside the plan", mutate: func(o *client.ContainerCreateOptions) {
		o.HostConfig.SecurityOpt = replaceOpt(o.HostConfig.SecurityOpt, "apparmor=", "apparmor=other")
		o.Config.Labels[LabelSpec] = Spec + ";apparmor=other"
	}, field: "HostConfig.SecurityOpt"},
	{name: "two apparmor options", mutate: hostOf(func(h *container.HostConfig) { h.SecurityOpt = append(h.SecurityOpt, optUnconfined) }), field: "HostConfig.SecurityOpt"},
	{name: "a fourth mount", mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts = append(h.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/bp/profiles/google/files", Target: "/more"})
	}), field: "HostConfig.Mounts"},
	{name: "the Docker socket", mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts[2] = mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/files"}
	}), field: "HostConfig.Mounts[2].Source"},
	{name: "the Docker socket at its own target", mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts[2] = mount.Mount{Type: mount.TypeBind, Source: "/var/run/docker.sock", Target: "/var/run/docker.sock"}
	}), field: "HostConfig.Mounts[2].Target"},
	{name: "a target twice", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[1].Target = "/profile" }), field: "HostConfig.Mounts[1].Target"},
	{name: "the data dir itself", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/srv/bp" }), field: "HostConfig.Mounts[0].Source"},
	{name: "the host's root", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[1].Source = "/" }), field: "HostConfig.Mounts[1].Source"},
	{name: "another profile's chrome folder", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/srv/bp/profiles/bank/chrome" }), field: "HostConfig.Mounts[0].Source"},
	{name: "a .. to another profile", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/srv/bp/profiles/google/../bank/chrome" }), field: "HostConfig.Mounts[0].Source"},
	{name: "chrome folder at /files", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Target, h.Mounts[2].Target = "/files", "/profile" }), field: "HostConfig.Mounts[0].Source"},
	{name: "a volume", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Type = mount.TypeVolume }), field: "HostConfig.Mounts[0].Type"},
	{name: "read-only", mutate: hostOf(func(h *container.HostConfig) { h.Mounts[1].ReadOnly = true }), field: "HostConfig.Mounts[1].ReadOnly"},
	{name: "bind propagation", mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts[0].BindOptions = &mount.BindOptions{Propagation: mount.PropagationRShared}
	}), field: "HostConfig.Mounts[0].BindOptions"},
	{name: "Binds", mutate: hostOf(func(h *container.HostConfig) { h.Binds = []string{"/:/host"} }), field: "HostConfig.Binds"},
	{name: "ExposedPorts", mutate: cfgOf(func(c *container.Config) { c.ExposedPorts = network.PortSet{network.MustParsePort("9222/tcp"): {}} }), field: "Config.ExposedPorts"},
	{name: "PortBindings", mutate: hostOf(func(h *container.HostConfig) {
		h.PortBindings = network.PortMap{network.MustParsePort("9222/tcp"): {{HostPort: "9222"}}}
	}), field: "HostConfig.PortBindings"},
	{name: "PublishAllPorts", mutate: hostOf(func(h *container.HostConfig) { h.PublishAllPorts = true }), field: "HostConfig.PublishAllPorts"},
	{name: "NetworkMode host", mutate: hostOf(func(h *container.HostConfig) { h.NetworkMode = "host" }), field: "HostConfig.NetworkMode"},
	{name: "NetworkMode container", mutate: hostOf(func(h *container.HostConfig) { h.NetworkMode = "container:x" }), field: "HostConfig.NetworkMode"},
	{name: "NetworkingConfig", mutate: func(o *client.ContainerCreateOptions) {
		o.NetworkingConfig = &network.NetworkingConfig{EndpointsConfig: map[string]*network.EndpointSettings{"host": {}}}
	}, field: "ContainerCreateOptions.NetworkingConfig"},
	{name: "PidMode host", mutate: hostOf(func(h *container.HostConfig) { h.PidMode = "host" }), field: "HostConfig.PidMode"},
	{name: "IpcMode host", mutate: hostOf(func(h *container.HostConfig) { h.IpcMode = "host" }), field: "HostConfig.IpcMode"},
	{name: "UTSMode host", mutate: hostOf(func(h *container.HostConfig) { h.UTSMode = "host" }), field: "HostConfig.UTSMode"},
	{name: "UsernsMode host", mutate: hostOf(func(h *container.HostConfig) { h.UsernsMode = "host" }), field: "HostConfig.UsernsMode"},
	{name: "CgroupnsMode host", mutate: hostOf(func(h *container.HostConfig) { h.CgroupnsMode = "host" }), field: "HostConfig.CgroupnsMode"},
	{name: "Devices", mutate: hostOf(func(h *container.HostConfig) {
		h.Devices = []container.DeviceMapping{{PathOnHost: "/dev/dri/renderD128", PathInContainer: "/dev/dri/renderD128", CgroupPermissions: "rw"}}
	}), field: "HostConfig.Devices"},
	{name: "DeviceRequests", mutate: hostOf(func(h *container.HostConfig) {
		h.DeviceRequests = []container.DeviceRequest{{Driver: "nvidia", Count: -1}}
	}), field: "HostConfig.DeviceRequests"},
	{name: "DeviceCgroupRules", mutate: hostOf(func(h *container.HostConfig) { h.DeviceCgroupRules = []string{"c 226:* rwm"} }), field: "HostConfig.DeviceCgroupRules"},
	{name: "Sysctls", mutate: hostOf(func(h *container.HostConfig) { h.Sysctls = map[string]string{"kernel.unprivileged_userns_clone": "1"} }), field: "HostConfig.Sysctls"},
	{name: "Tmpfs", mutate: hostOf(func(h *container.HostConfig) { h.Tmpfs = map[string]string{"/tmp": "exec"} }), field: "HostConfig.Tmpfs"},
	{name: "VolumesFrom", mutate: hostOf(func(h *container.HostConfig) { h.VolumesFrom = []string{"browserpier"} }), field: "HostConfig.VolumesFrom"},
	{name: "ExtraHosts", mutate: hostOf(func(h *container.HostConfig) { h.ExtraHosts = []string{"host.docker.internal:host-gateway"} }), field: "HostConfig.ExtraHosts"},
	{name: "DNS", mutate: hostOf(func(h *container.HostConfig) { h.DNS = []netip.Addr{netip.MustParseAddr("1.1.1.1")} }), field: "HostConfig.DNS"},
	{name: "Runtime", mutate: hostOf(func(h *container.HostConfig) { h.Runtime = "runc-unsafe" }), field: "HostConfig.Runtime"},
	{name: "RestartPolicy", mutate: hostOf(func(h *container.HostConfig) { h.RestartPolicy = container.RestartPolicy{Name: "always"} }), field: "HostConfig.RestartPolicy"},
	{name: "Memory", mutate: hostOf(func(h *container.HostConfig) { h.Memory = 1 << 30 }), field: "HostConfig.Memory"},
	{name: "MaskedPaths empty", mutate: hostOf(func(h *container.HostConfig) { h.MaskedPaths = []string{} }), field: "HostConfig.MaskedPaths"},
	{name: "ReadonlyPaths empty", mutate: hostOf(func(h *container.HostConfig) { h.ReadonlyPaths = []string{} }), field: "HostConfig.ReadonlyPaths"},
	{name: "Cmd on a browser", mutate: cfgOf(func(c *container.Config) { c.Cmd = []string{"sandbox-check"} }), field: "Config.Cmd"},
	{name: "another Cmd on the check", kind: kindSandboxCheck, mutate: cfgOf(func(c *container.Config) { c.Cmd = []string{"sh"} }), field: "Config.Cmd"},
	{name: "no Cmd on the check", kind: kindSandboxCheck, mutate: cfgOf(func(c *container.Config) { c.Cmd = nil }), field: "Config.Cmd"},
	{name: "Entrypoint", mutate: cfgOf(func(c *container.Config) { c.Entrypoint = []string{"/bin/sh"} }), field: "Config.Entrypoint"},
	{name: "an unknown environment name", mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "LD_PRELOAD=/files/x.so") }), field: "Config.Env"},
	// Read by the entrypoint, never set by the orchestrator: a test sets
	// them in a test image's own ENV, not in a create body (PROJ-208).
	{name: "the chooser timeout", mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "BROWSERPIER_CHOOSER_TIMEOUT=3s") }), field: "Config.Env"},
	{name: "the stop timeout", mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "BROWSERPIER_STOP_TIMEOUT=1s") }), field: "Config.Env"},
	{name: "a duplicate environment name", mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "BROWSERPIER_SCREEN=800x600") }), field: "Config.Env"},
	{name: "a newline in a value", mutate: func(o *client.ContainerCreateOptions) {
		setEnv(o, "BROWSERPIER_LOCALE_OVERRIDE", "fr-FR\nBROWSERPIER_BLOCK_ADS=false")
	}, field: "Config.Env"},
	{name: "a NUL in a value", mutate: func(o *client.ContainerCreateOptions) { setEnv(o, "BROWSERPIER_GEO_TZ", "Europe/Paris\x00") }, field: "Config.Env"},
	{name: "the mode twice", mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "BROWSERPIER_MODE=api") }), field: "Config.Env"},
	{name: "a newline in the mode", mutate: func(o *client.ContainerCreateOptions) {
		setEnv(o, "BROWSERPIER_MODE", "visual\nLD_PRELOAD=/files/x.so")
	}, field: "Config.Env"},
	{name: "no equals sign", mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "BROWSERPIER_BLOCK_ADS") }), field: "Config.Env"},
	{name: "a required name missing", mutate: cfgOf(func(c *container.Config) { c.Env = c.Env[1:] }), field: "Config.Env"},
	{name: "environment on the check", kind: kindSandboxCheck, mutate: cfgOf(func(c *container.Config) { c.Env = []string{"BROWSERPIER_SCREEN=1x1"} }), field: "Config.Env"},
	{name: "a missing label", mutate: cfgOf(func(c *container.Config) { delete(c.Labels, LabelSession) }), field: "Config.Labels"},
	{name: "a missing label, another added", mutate: cfgOf(func(c *container.Config) { delete(c.Labels, LabelSession); c.Labels["x"] = "y" }), field: "Config.Labels"},
	{name: "an extra label", mutate: cfgOf(func(c *container.Config) { c.Labels["com.example"] = "x" }), field: "Config.Labels"},
	{name: "another instance", mutate: cfgOf(func(c *container.Config) { c.Labels[LabelInstance] = "i2" }), field: "Config.Labels"},
	{name: "another instance on the check", kind: kindSandboxCheck, mutate: cfgOf(func(c *container.Config) { c.Labels[LabelInstance] = "i2" }), field: "Config.Labels"},
	{name: "a profile outside the pattern", mutate: cfgOf(func(c *container.Config) { c.Labels[LabelProfile] = "../bank" }), field: "Config.Labels"},
	{name: "an empty session", mutate: cfgOf(func(c *container.Config) { c.Labels[LabelSession] = "" }), field: "Config.Labels"},
	{name: "not managed", mutate: cfgOf(func(c *container.Config) { c.Labels[LabelManaged] = "false" }), field: "Config.Labels"},
	{name: "a spec label that disagrees", mutate: cfgOf(func(c *container.Config) { c.Labels[LabelSpec] = Spec }), field: "Config.Labels"},
	{name: "an old spec label", mutate: cfgOf(func(c *container.Config) { c.Labels[LabelSpec] = "3;" + optBrowserPier }), field: "Config.Labels"},
	{name: "the profile and the name disagree", mutate: func(o *client.ContainerCreateOptions) { o.Name = "browserpier-bank" }, field: "ContainerCreateOptions.Name"},
	{name: "the check's name", kind: kindSandboxCheck, mutate: func(o *client.ContainerCreateOptions) { o.Name = "browserpier-google" }, field: "ContainerCreateOptions.Name"},
	{name: "Init nil", mutate: hostOf(func(h *container.HostConfig) { h.Init = nil }), field: "HostConfig.Init"},
	{name: "Init true", mutate: hostOf(func(h *container.HostConfig) { h.Init = &tr }), field: "HostConfig.Init"},
	{name: "Init true on the check", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.Init = &tr }), field: "HostConfig.Init"},
	{name: "ShmSize changed", mutate: hostOf(func(h *container.HostConfig) { h.ShmSize = 2 << 30 }), field: "HostConfig.ShmSize"},
	{name: "ShmSize on the check", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.ShmSize = 1 << 30 }), field: "HostConfig.ShmSize"},
	{name: "StopTimeout changed", mutate: cfgOf(func(c *container.Config) { c.StopTimeout = &other }), field: "Config.StopTimeout"},
	{name: "StopTimeout unset", mutate: cfgOf(func(c *container.Config) { c.StopTimeout = nil }), field: "Config.StopTimeout"},
	{name: "StopTimeout on the check", kind: kindSandboxCheck, mutate: cfgOf(func(c *container.Config) { c.StopTimeout = &other }), field: "Config.StopTimeout"},
	{name: "a mount on the check", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: "/srv/bp/profiles/google/chrome", Target: "/profile"}}
	}), field: "HostConfig.Mounts"},
	{name: "a network for the check", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.NetworkMode = "" }), field: "HostConfig.NetworkMode"},
	// Limits belong to the proxy test alone.
	{name: "MemorySwap on a browser", mutate: hostOf(func(h *container.HostConfig) { h.MemorySwap = 1 << 30 }), field: "HostConfig.MemorySwap"},
	{name: "PidsLimit on a browser", mutate: hostOf(func(h *container.HostConfig) { h.PidsLimit = pidsOf(64) }), field: "HostConfig.PidsLimit"},
	{name: "the proxy test's limits on a browser", mutate: hostOf(proxyLimits), field: "HostConfig.Memory"},
	{name: "Memory on the check", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.Memory = 128 << 20 }), field: "HostConfig.Memory"},
	{name: "PidsLimit on the check", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.PidsLimit = pidsOf(64) }), field: "HostConfig.PidsLimit"},
	{name: "the check's CapAdd emptied", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.CapAdd = nil }), field: "HostConfig.CapAdd"},
	{name: "the check's CapAdd widened", kind: kindSandboxCheck, mutate: hostOf(func(h *container.HostConfig) { h.CapAdd = append(h.CapAdd, "NET_ADMIN") }), field: "HostConfig.CapAdd"},

	// The proxy test (PROJ-154).
	{name: "another image on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Image = "evil/chromium:latest" }), field: "Config.Image"},
	{name: "root on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.User = "0:0" }), field: "Config.User"},
	{name: "UsernsMode host on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.UsernsMode = "host" }), field: "HostConfig.UsernsMode"},
	{name: "CapDrop emptied on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.CapDrop = nil }), field: "HostConfig.CapDrop"},
	{name: "SYS_CHROOT on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.CapAdd = []string{"SYS_CHROOT"} }), field: "HostConfig.CapAdd"},
	{name: "an empty CapAdd on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.CapAdd = []string{} }), field: "HostConfig.CapAdd"},
	{name: "no seccomp on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) {
		h.SecurityOpt = replaceOpt(h.SecurityOpt, "seccomp=", "")
	}), field: "HostConfig.SecurityOpt"},
	{name: "no no-new-privileges on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) {
		h.SecurityOpt = replaceOpt(h.SecurityOpt, "no-new-privileges", "")
	}), field: "HostConfig.SecurityOpt"},
	{name: "Init true on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Init = &tr }), field: "HostConfig.Init"},
	{name: "Privileged on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Privileged = true }), field: "HostConfig.Privileged"},
	{name: "PortBindings on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) {
		h.PortBindings = network.PortMap{network.MustParsePort("9222/tcp"): {{HostPort: "9222"}}}
	}), field: "HostConfig.PortBindings"},
	{name: "managed, as a browser", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Labels[LabelManaged] = "true" }), field: "Config.Labels"},
	{name: "the proxy test unlabelled", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { delete(c.Labels, LabelProxyTest) }), field: "Config.Labels"},
	{name: "the proxy test label false", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Labels[LabelProxyTest] = "false" }), field: "Config.Labels"},
	{name: "another instance on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Labels[LabelInstance] = "i2" }), field: "Config.Labels"},
	{name: "the check's labels on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) {
		c.Labels = map[string]string{LabelCheck: "true", LabelInstance: "i1"}
	}), field: "Config.Labels"},
	{name: "a browser's name on the proxy test", kind: kindProxyTest, mutate: func(o *client.ContainerCreateOptions) { o.Name = "browserpier-google" }, field: "ContainerCreateOptions.Name"},
	{name: "the check's name on the proxy test", kind: kindProxyTest, mutate: func(o *client.ContainerCreateOptions) { o.Name = "browserpier_check_i1" }, field: "ContainerCreateOptions.Name"},
	{name: "no environment on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Env = nil }), field: "Config.Env"},
	{name: "a second variable on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, "BROWSERPIER_SCREEN=1x1") }), field: "Config.Env"},
	{name: "the endpoint twice on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Env = append(c.Env, c.Env[0]) }), field: "Config.Env"},
	{name: "the proxy in the environment", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) {
		c.Env = []string{"HTTPS_PROXY=http://user:pw@evil.example:8080"}
	}), field: "Config.Env"},
	{name: "a browser variable in place of the endpoint", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Env = []string{"BROWSERPIER_GEO_TZ=Europe/Paris"} }), field: "Config.Env"},
	{name: "an empty endpoint", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Env = []string{"BROWSERPIER_GEOIP_URL="} }), field: "Config.Env"},
	{name: "no equals sign on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Env = []string{"BROWSERPIER_GEOIP_URL"} }), field: "Config.Env"},
	{name: "a newline in the endpoint", kind: kindProxyTest, mutate: func(o *client.ContainerCreateOptions) {
		setEnv(o, "BROWSERPIER_GEOIP_URL", "https://ipinfo.io/json\nBROWSERPIER_SCREEN=1x1")
	}, field: "Config.Env"},
	{name: "a CR in the endpoint", kind: kindProxyTest, mutate: func(o *client.ContainerCreateOptions) { setEnv(o, "BROWSERPIER_GEOIP_URL", "https://ipinfo.io/json\r") }, field: "Config.Env"},
	{name: "a NUL in the endpoint", kind: kindProxyTest, mutate: func(o *client.ContainerCreateOptions) {
		setEnv(o, "BROWSERPIER_GEOIP_URL", "https://ipinfo.io/json\x00")
	}, field: "Config.Env"},
	{name: "no Cmd on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Cmd = nil }), field: "Config.Cmd"},
	{name: "the sandbox check's Cmd on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Cmd = []string{"sandbox-check"} }), field: "Config.Cmd"},
	{name: "an argument after proxy-check", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Cmd = append(c.Cmd, "--debug") }), field: "Config.Cmd"},
	{name: "Entrypoint on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.Entrypoint = []string{"/bin/sh"} }), field: "Config.Entrypoint"},
	{name: "StopTimeout on the proxy test", kind: kindProxyTest, mutate: cfgOf(func(c *container.Config) { c.StopTimeout = &other }), field: "Config.StopTimeout"},
	{name: "NetworkMode none on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.NetworkMode = "none" }), field: "HostConfig.NetworkMode"},
	{name: "NetworkMode host on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.NetworkMode = "host" }), field: "HostConfig.NetworkMode"},
	{name: "ShmSize on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.ShmSize = 1 << 30 }), field: "HostConfig.ShmSize"},
	{name: "no memory limit on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Memory = 0 }), field: "HostConfig.Memory"},
	{name: "more memory on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Memory, h.MemorySwap = 1<<30, 1<<30 }), field: "HostConfig.Memory"},
	{name: "swap on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.MemorySwap = 2 * ProxyTestMemory }), field: "HostConfig.MemorySwap"},
	{name: "unlimited swap on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.MemorySwap = -1 }), field: "HostConfig.MemorySwap"},
	{name: "no MemorySwap on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.MemorySwap = 0 }), field: "HostConfig.MemorySwap"},
	{name: "no pids limit on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.PidsLimit = nil }), field: "HostConfig.PidsLimit"},
	{name: "more pids on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.PidsLimit = pidsOf(65) }), field: "HostConfig.PidsLimit"},
	{name: "unlimited pids on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.PidsLimit = pidsOf(-1) }), field: "HostConfig.PidsLimit"},
	{name: "another Resources field on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.NanoCPUs = 1 }), field: "HostConfig.NanoCPUs"},
	{name: "no mount on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts = nil }), field: "HostConfig.Mounts"},
	{name: "a browser's mounts on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts = slices.Clone(browserMounts)
	}), field: "HostConfig.Mounts"},
	{name: "a second mount on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) {
		h.Mounts = append(h.Mounts, mount.Mount{Type: mount.TypeBind, Source: "/srv/bp/proxytest", Target: "/files"})
	}), field: "HostConfig.Mounts"},
	{name: "a profile's run folder on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/srv/bp/profiles/google/run" }), field: "HostConfig.Mounts[0].Source"},
	{name: "the data dir on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/srv/bp" }), field: "HostConfig.Mounts[0].Source"},
	{name: "a .. out of the proxy test's folder", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/srv/bp/proxytest/../profiles" }), field: "HostConfig.Mounts[0].Source"},
	{name: "the Docker socket on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Source = "/var/run/docker.sock" }), field: "HostConfig.Mounts[0].Source"},
	{name: "the proxy test's folder at /profile", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Target = "/profile" }), field: "HostConfig.Mounts[0].Target"},
	{name: "a volume on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].Type = mount.TypeVolume }), field: "HostConfig.Mounts[0].Type"},
	{name: "read-only on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Mounts[0].ReadOnly = true }), field: "HostConfig.Mounts[0].ReadOnly"},
	{name: "Binds on the proxy test", kind: kindProxyTest, mutate: hostOf(func(h *container.HostConfig) { h.Binds = []string{"/:/host"} }), field: "HostConfig.Binds"},
	{name: "Config unset", mutate: func(o *client.ContainerCreateOptions) { o.Config = nil }, field: "Config"},
	{name: "HostConfig unset", mutate: func(o *client.ContainerCreateOptions) { o.HostConfig = nil }, field: "HostConfig"},
}

// body builds a fresh valid body of kind with the real builders.
func (w widening) body(c *Client) client.ContainerCreateOptions {
	return bodyOf(c, w.kind, w.opt)
}

func (w widening) client(t *testing.T) (*Client, createKind) {
	plan := appArmorPlan{try: []string{optBrowserPier, "", optUnconfined}, fallback: optUnconfined}
	if w.plan != nil {
		plan = *w.plan
	}
	kind := w.kind
	if kind == "" {
		kind = kindBrowser
	}
	return guardClient(t, false, plan), kind
}

func TestGuardRefusesEachWidening(t *testing.T) {
	for _, w := range widenings {
		if w.opt == "" && w.plan == nil {
			w.opt = optBrowserPier
		}
		c, kind := w.client(t)
		o := w.body(c)
		if err := c.checkCreate(kind, o); err != nil {
			t.Fatalf("%s: the body before the change is refused: %v", w.name, err)
		}
		w.mutate(&o)
		err := c.checkCreate(kind, o)
		if !errors.Is(err, ErrOutsideEnvelope) {
			t.Errorf("%s: not refused (%v)", w.name, err)
			continue
		}
		prefix := "refused to create the " + string(kind) + " container: " + w.field + " "
		if !strings.HasPrefix(err.Error(), prefix) {
			t.Errorf("%s: %q doesn't name %s", w.name, err, w.field)
		}
	}
}

// fill sets v, a settable field, to a value other than its zero value. A
// non-nil empty slice or map is already one.
func fill(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Bool:
		v.SetBool(true)
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		v.SetUint(1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(1)
	case reflect.String:
		v.SetString("x")
	case reflect.Slice:
		v.Set(reflect.MakeSlice(v.Type(), 0, 0))
	case reflect.Map:
		v.Set(reflect.MakeMap(v.Type()))
	case reflect.Pointer:
		v.Set(reflect.New(v.Type().Elem()))
	case reflect.Array:
		return v.Len() > 0 && fill(v.Index(0))
	case reflect.Struct:
		for i := range v.NumField() {
			if v.Field(i).CanSet() && fill(v.Field(i)) {
				return true
			}
		}
		return false
	default:
		return false
	}
	return !v.IsZero()
}

func TestGuardRefusesAnyUnnamedField(t *testing.T) {
	c := guardClient(t, false, appArmorPlan{})
	type target struct {
		typ    reflect.Type
		prefix string
		kinds  []createKind
		get    func(o *client.ContainerCreateOptions) reflect.Value
	}
	both := allKinds
	targets := []target{
		{reflect.TypeFor[client.ContainerCreateOptions](), "ContainerCreateOptions", both, func(o *client.ContainerCreateOptions) reflect.Value { return reflect.ValueOf(o).Elem() }},
		{reflect.TypeFor[container.Config](), "Config", both, func(o *client.ContainerCreateOptions) reflect.Value { return reflect.ValueOf(o.Config).Elem() }},
		{reflect.TypeFor[container.HostConfig](), "HostConfig", both, func(o *client.ContainerCreateOptions) reflect.Value { return reflect.ValueOf(o.HostConfig).Elem() }},
		{reflect.TypeFor[container.Resources](), "HostConfig", both, func(o *client.ContainerCreateOptions) reflect.Value {
			return reflect.ValueOf(&o.HostConfig.Resources).Elem()
		}},
		{reflect.TypeFor[mount.Mount](), "HostConfig.Mounts[0]", []createKind{kindBrowser, kindProxyTest}, func(o *client.ContainerCreateOptions) reflect.Value {
			return reflect.ValueOf(&o.HostConfig.Mounts[0]).Elem()
		}},
	}
	checked := 0
	for _, tg := range targets {
		named := guardNamed[tg.typ]
		for i := range tg.typ.NumField() {
			f := tg.typ.Field(i)
			if slices.Contains(named, f.Name) || f.Anonymous && f.Type == reflect.TypeFor[container.Resources]() {
				continue // checked by value, or walked as Resources
			}
			for _, kind := range tg.kinds {
				o := bodyOf(c, kind, "")
				fv := tg.get(&o).Field(i)
				if !fv.CanSet() || !fill(fv) {
					t.Errorf("%s.%s (%s): the test can't set it; teach fill its type", tg.prefix, f.Name, f.Type)
					continue
				}
				err := c.checkCreate(kind, o)
				want := "refused to create the " + string(kind) + " container: " + tg.prefix + "." + f.Name + " is set"
				if !errors.Is(err, ErrOutsideEnvelope) || err.Error() != want {
					t.Errorf("%s.%s on a %s: got %v, want %q", tg.prefix, f.Name, kind, err, want)
				}
				checked++
			}
		}
	}
	t.Logf("%d unnamed fields refused", checked)
	if checked < 200 {
		t.Fatalf("only %d fields checked: the walk misses some", checked)
	}
}

// The fields checked by value exist in the SDK, so a rename fails here.
func TestGuardNamedFieldsExist(t *testing.T) {
	for typ, names := range guardNamed {
		if typ.Kind() != reflect.Struct {
			t.Errorf("%s isn't a struct", typ)
		}
		for _, n := range names {
			if _, ok := typ.FieldByName(n); !ok {
				t.Errorf("%s has no field %s", typ, n)
			}
		}
	}
	// HostConfig embeds Resources, which the walk must reach.
	f, ok := reflect.TypeFor[container.HostConfig]().FieldByName("Resources")
	if !ok || !f.Anonymous {
		t.Fatal("HostConfig no longer embeds Resources: update the guard's walk")
	}
	// Item 5 of PROJ-159: with moby/api v1.56.1, MaskedPaths and
	// ReadonlyPaths have no omitempty, so an empty list reaches the daemon
	// as [] and unmasks every path, where nil keeps the defaults.
	for _, n := range []string{"MaskedPaths", "ReadonlyPaths"} {
		f, _ := reflect.TypeFor[container.HostConfig]().FieldByName(n)
		if strings.Contains(f.Tag.Get("json"), "omitempty") {
			t.Errorf("%s gained omitempty; the guard still refuses an empty list", n)
		}
	}
}

func TestGuardKindsDontMix(t *testing.T) {
	c := guardClient(t, false, appArmorPlan{})
	for _, as := range allKinds {
		for _, of := range allKinds {
			if as == of {
				continue
			}
			if err := c.checkCreate(as, bodyOf(c, of, "")); !errors.Is(err, ErrOutsideEnvelope) {
				t.Errorf("the %s's body as a %s: %v", of, as, err)
			}
		}
	}
	browser := c.browserCreateOptions("", goldenSpec())
	for _, kind := range []createKind{"", "proxy_test", "Proxy-test", "Browser"} {
		if err := c.checkCreate(kind, browser); !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "the kind is unknown") {
			t.Errorf("kind %q: %v", kind, err)
		}
	}
}

// The nit of the Codex review: an unknown kind is shown as "unknown", in
// the error and the log, never as the string it was given.
func TestUnknownKindIsntEchoed(t *testing.T) {
	d := &createDaemon{}
	c, logs := goldenClient(t, d, false, optBrowserPier)
	const odd = "secret-s3cr3t"
	_, err := c.create(context.Background(), createKind(odd), c.browserCreateOptions(optBrowserPier, goldenSpec()))
	if !errors.Is(err, ErrOutsideEnvelope) || err.Error() != "refused to create the unknown container: the kind is unknown" {
		t.Fatalf("create = %v", err)
	}
	if !strings.Contains(logs.String(), "kind=unknown") {
		t.Fatalf("the log doesn't say kind=unknown:\n%s", logs)
	}
	if strings.Contains(logs.String(), odd) {
		t.Fatalf("the log echoes the kind:\n%s", logs)
	}
	if creates, _ := d.counts(); creates != 0 {
		t.Fatalf("%d creates reached the daemon", creates)
	}
}

func TestGuardRefusesABrowserBeforeUseDataDir(t *testing.T) {
	c := guardClient(t, false, appArmorPlan{})
	o := c.browserCreateOptions("", goldenSpec())
	c.hostDataDir = ""
	if err := c.checkCreate(kindBrowser, o); !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "UseDataDir") {
		t.Fatalf("a browser before UseDataDir: %v", err)
	}
	// Nor a relative one, which Clean makes of "" (".").
	for _, dir := range []string{".", "relative/dir"} {
		c = guardClient(t, false, appArmorPlan{})
		c.hostDataDir = dir
		spec := goldenSpec()
		spec.ProfileDir, spec.ControlDir, spec.FilesDir = filepath.Join(dir, "profiles/google/chrome"), filepath.Join(dir, "profiles/google/run"), filepath.Join(dir, "profiles/google/files")
		err := c.checkCreate(kindBrowser, c.browserCreateOptions("", spec))
		if !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "HostConfig.Mounts") {
			t.Errorf("host data dir %q: %v", dir, err)
		}
		assertNoValues(t, dir, err.Error())
	}
	// Nor the proxy test, whose one mount is computed from it too.
	for _, dir := range []string{"", ".", "relative/dir"} {
		c = guardClient(t, false, appArmorPlan{})
		c.hostDataDir = dir
		err := c.checkCreate(kindProxyTest, c.proxyTestOptions("", goldenProxyTest(c)))
		if !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "HostConfig.Mounts can't be checked before UseDataDir") {
			t.Errorf("the proxy test, host data dir %q: %v", dir, err)
		}
		assertNoValues(t, dir, err.Error())
	}
	// Podman's seccomp path can't be checked either.
	for _, dir := range []string{"", ".", "relative/dir"} {
		c = guardClient(t, true, appArmorPlan{})
		c.hostDataDir, c.seccompOpt = dir, "seccomp="+filepath.Join(dir, "seccomp.json")
		err := c.checkCreate(kindSandboxCheck, c.sandboxCheckOptions(""))
		if !errors.Is(err, ErrOutsideEnvelope) {
			t.Fatalf("Podman's seccomp path under %q: %v", dir, err)
		}
		assertNoValues(t, dir, err.Error())
	}
	// UseDataDir refuses one before asking the daemon anything.
	c = guardClient(t, false, appArmorPlan{})
	for _, dir := range []string{"", "relative/dir"} {
		if err := c.UseDataDir(context.Background(), t.TempDir(), dir); err == nil {
			t.Errorf("UseDataDir took the host data dir %q", dir)
		}
	}
}

// The seccomp option must be the profile BrowserPier ships, in Docker's or
// Podman's form, whatever the client was given.
func TestGuardChecksTheClientsOwnSettings(t *testing.T) {
	for name, tweak := range map[string]func(c *Client){
		"seccomp unconfined":           func(c *Client) { c.seccompOpt = "seccomp=unconfined" },
		"another inline profile":       func(c *Client) { c.seccompOpt = `seccomp={"defaultAction":"SCMP_ACT_ALLOW"}` },
		"a path outside the data dir":  func(c *Client) { c.seccompOpt = "seccomp=/tmp/seccomp.json" },
		"userns host":                  func(c *Client) { c.userns = "host" },
		"userns keep-id with a letter": func(c *Client) { c.userns = "keep-id:uid=x" },
		"no image":                     func(c *Client) { c.image = "" },
		"no instance":                  func(c *Client) { c.instance = "" },
		"root user":                    func(c *Client) { c.user = "0:0" },
	} {
		c := guardClient(t, false, appArmorPlan{})
		tweak(c)
		for _, kind := range allKinds {
			o := bodyOf(c, kind, "")
			if name == "root user" {
				o.Config.User = "0:0"
			}
			err := c.checkCreate(kind, o)
			if !errors.Is(err, ErrOutsideEnvelope) {
				t.Errorf("%s, %s: %v", name, kind, err)
				continue
			}
			assertNoValues(t, name, err.Error())
		}
	}
}

// guardValues are values the refusals in these tests are about; no error
// or log record may show one.
var guardValues = []string{"SCMP", "defaultAction", "/", "srv", "docker.sock", "1440x900", "ipinfo", "Paris", "fr-FR", "192.168", "evil", "9222",
	"LD_PRELOAD", "my-browsers", "bank", "1000", "unconfined", "tmp", "uid=x", "relative"}

func assertNoValues(t *testing.T, what string, texts ...string) {
	t.Helper()
	for _, s := range texts {
		for _, v := range guardValues {
			if strings.Contains(s, v) {
				t.Errorf("%s: %q shows %q", what, s, v)
			}
		}
	}
}

func TestGuardErrorNamesFieldsNotValues(t *testing.T) {
	values := guardValues
	for _, w := range widenings {
		if w.opt == "" && w.plan == nil {
			w.opt = optBrowserPier
		}
		c, kind := w.client(t)
		var logs bytes.Buffer
		c.log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{ReplaceAttr: func(_ []string, a slog.Attr) slog.Attr {
			if a.Key == slog.TimeKey {
				return slog.Attr{}
			}
			return a
		}}))
		o := w.body(c)
		w.mutate(&o)
		_, err := c.create(context.Background(), kind, o) // refused before the (nil) daemon
		if err == nil {
			t.Fatalf("%s: not refused", w.name)
		}
		if n := strings.Count(logs.String(), "level=ERROR"); n != 1 {
			t.Errorf("%s: logged %d times at Error:\n%s", w.name, n, logs.String())
		}
		for _, v := range values {
			if strings.Contains(err.Error(), v) || strings.Contains(logs.String(), v) {
				t.Errorf("%s: the error or log shows %q: %v\n%s", w.name, v, err, logs.String())
			}
		}
	}
}

func TestRefusedCreateNeverReachesTheDaemon(t *testing.T) {
	ctx := context.Background()
	// A browser whose chrome folder is the data dir: refused before the
	// daemon, and the image missing doesn't lead to a pull.
	d := &createDaemon{missing: true}
	c, logs := goldenClient(t, d, false, optBrowserPier)
	spec := goldenSpec()
	spec.ProfileDir = "/srv/bp"
	_, err := c.Create(ctx, spec)
	if !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "HostConfig.Mounts[0].Source") {
		t.Fatalf("Create = %v", err)
	}
	if creates, pulls := d.counts(); creates != 0 || pulls != 0 {
		t.Fatalf("a refused body reached the daemon: %d creates, %d pulls", creates, pulls)
	}
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("the refusal wasn't logged:\n%s", logs)
	}
	// An accepted one: exactly one create, with the body sent before the
	// guard.
	d = &createDaemon{}
	c, _ = goldenClient(t, d, false, optBrowserPier)
	if _, err := c.Create(ctx, goldenSpec()); err != nil {
		t.Fatal(err)
	}
	if creates, pulls := d.counts(); creates != 1 || pulls != 0 {
		t.Fatalf("%d creates, %d pulls", creates, pulls)
	}
	checkGolden(t, "create-browser-docker.json", d.creates[0])
}

// Item 1 of the Codex review: with the AppArmor choice unsettled and the
// image missing, choosing the option pulls the image. A body the guard
// refuses must be refused before that.
func TestRefusedCreateDoesntPullWhileAppArmorIsUnsettled(t *testing.T) {
	d := &createDaemon{missing: true, noImage: true}
	c, _ := goldenClient(t, d, false, optUnconfined)
	c.apparmorSettled = false
	spec := goldenSpec()
	spec.FilesDir = "/srv/bp/profiles/other/files"
	_, err := c.Create(context.Background(), spec)
	if !errors.Is(err, ErrOutsideEnvelope) || !strings.Contains(err.Error(), "HostConfig.Mounts[2].Source") {
		t.Fatalf("Create = %v", err)
	}
	if creates, pulls := d.counts(); creates != 0 || pulls != 0 {
		t.Fatalf("a refused body reached the daemon: %d creates, %d pulls; requests %q", creates, pulls, d.requests)
	}
	for _, r := range d.requests {
		if strings.Contains(r, "/images/") {
			t.Fatalf("a refused body led to %s", r)
		}
	}
}

func TestRefusalIsNotAnAppArmorVerdict(t *testing.T) {
	ctx := context.Background()
	// An AppArmor option outside the plan: the refusal names AppArmor,
	// and must still not read as "this profile doesn't work".
	d := &createDaemon{}
	c, _ := goldenClient(t, d, false, "")
	c.plan = appArmorPlan{}
	v, why := c.checkSandbox(ctx, optBrowserPier)
	if v != unknown || !strings.Contains(why, "refused to create the sandbox-check container") {
		t.Fatalf("checkSandbox = %v, %q; want unknown", v, why)
	}
	// And an empty user, say.
	c.plan = appArmorPlan{try: []string{optBrowserPier}}
	c.user = ""
	if v, why := c.checkSandbox(ctx, optBrowserPier); v != unknown || !strings.Contains(why, "Config.User") {
		t.Fatalf("checkSandbox = %v, %q; want unknown", v, why)
	}
	if creates, _ := d.counts(); creates != 0 {
		t.Fatalf("%d creates reached the daemon", creates)
	}
	if appArmorVerdict(&envelopeError{kind: kindSandboxCheck, field: "HostConfig.SecurityOpt", what: "has an AppArmor option the plan doesn't allow"}) != unknown {
		t.Fatal("a refusal read as an AppArmor verdict")
	}
}

func TestSandboxCheckBody(t *testing.T) {
	c := guardClient(t, true, appArmorPlan{})
	o := c.sandboxCheckOptions(optUnconfined)
	cfg, host := o.Config, o.HostConfig
	if o.Name != "browserpier_check_i1" || o.Image != "" || o.Platform != nil || o.NetworkingConfig != nil {
		t.Fatalf("options %+v", o)
	}
	if cfg.Image != goldenImage || cfg.User != "1000:1000" || !slices.Equal(cfg.Cmd, []string{"sandbox-check"}) || cfg.Entrypoint != nil {
		t.Fatalf("image %q user %q cmd %q entrypoint %q", cfg.Image, cfg.User, cfg.Cmd, cfg.Entrypoint)
	}
	if cfg.Env != nil || cfg.StopTimeout != nil {
		t.Fatalf("env %q stop timeout %v; the check gets neither", cfg.Env, cfg.StopTimeout)
	}
	if !reflect.DeepEqual(cfg.Labels, map[string]string{LabelCheck: "true", LabelInstance: "i1"}) {
		t.Fatalf("labels %v", cfg.Labels)
	}
	if host.NetworkMode != "none" || host.Mounts != nil || host.Binds != nil || host.ShmSize != 0 || host.UsernsMode != "keep-id" {
		t.Fatalf("network %q mounts %v binds %v shm %d userns %q", host.NetworkMode, host.Mounts, host.Binds, host.ShmSize, host.UsernsMode)
	}
	// The browsers' confinement, which is what it checks.
	if !slices.Equal(host.CapDrop, []string{"ALL"}) || !slices.Equal(host.CapAdd, []string{"SYS_CHROOT"}) || host.Privileged {
		t.Fatalf("caps add %v drop %v privileged %t", host.CapAdd, host.CapDrop, host.Privileged)
	}
	if !slices.Equal(host.SecurityOpt, []string{"seccomp=/srv/bp/seccomp.json", "no-new-privileges:true", optUnconfined}) {
		t.Fatalf("security opts %v", host.SecurityOpt)
	}
	if host.Init == nil || *host.Init {
		t.Fatal("the image's entrypoint must be PID 1")
	}
	// Nothing else.
	restCfg, restHost := *cfg, *host
	restCfg.Image, restCfg.User, restCfg.Cmd, restCfg.Labels = "", "", nil, nil
	restHost.NetworkMode, restHost.UsernsMode, restHost.CapDrop, restHost.CapAdd, restHost.SecurityOpt, restHost.Init = "", "", nil, nil, nil, nil
	if !reflect.ValueOf(restCfg).IsZero() || !reflect.ValueOf(restHost).IsZero() {
		t.Fatalf("more is set than the check needs: %+v %+v", restCfg, restHost)
	}
}

// moduleRoot is the repository's root, from internal/docker.
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(err)
	}
	return root
}

// apiCalls parses this package's non-test files and returns, per file, the
// methods called on the SDK client (c.api.X), and any other use of it.
func apiCalls(t *testing.T) (calls map[string][]string, other []string) {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	calls = map[string][]string{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		// Selectors whose X is .api: c.api.Method.
		inner := map[*ast.SelectorExpr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.SelectorExpr); ok && x.Sel.Name == "api" {
				inner[x] = true
				calls[name] = append(calls[name], sel.Sel.Name)
			}
			if sel.Sel.Name == "ContainerCreate" {
				calls[name] = append(calls[name], "ContainerCreate (any selector)")
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == "api" && !inner[sel] {
				other = append(other, fset.Position(sel.Pos()).String())
			}
			return true
		})
	}
	return calls, other
}

func TestOnlyTheGuardCreatesContainers(t *testing.T) {
	calls, _ := apiCalls(t)
	n := 0
	for file, methods := range calls {
		for _, m := range methods {
			if m != "ContainerCreate" && m != "ContainerCreate (any selector)" {
				continue
			}
			if file != "guard.go" {
				t.Errorf("%s calls ContainerCreate; only guard.go may", file)
			}
			if m == "ContainerCreate" {
				n++
			}
		}
	}
	if n != 1 {
		t.Errorf("%d calls to c.api.ContainerCreate; want exactly one, in guard.go", n)
	}
}

func TestDockerOperationsAreAClosedSet(t *testing.T) {
	// Adding one (ContainerExecCreate, NetworkCreate, CopyToContainer…) is
	// a deliberate edit of this list, and of the guard if it creates.
	allowed := []string{"Close", "ContainerCreate", "ContainerInspect", "ContainerList", "ContainerLogs", "ContainerRemove",
		"ContainerStart", "ContainerStats", "ContainerStop", "ContainerWait", "ImageInspect", "ImagePull", "Info", "Ping", "ServerVersion"}
	calls, other := apiCalls(t)
	seen := 0
	for file, methods := range calls {
		for _, m := range methods {
			if m == "ContainerCreate (any selector)" {
				continue
			}
			seen++
			if !slices.Contains(allowed, m) {
				t.Errorf("%s calls c.api.%s, outside the closed set", file, m)
			}
		}
	}
	if seen == 0 {
		t.Fatal("found no calls: the parse is wrong")
	}
	// The client isn't handed around, where its calls would escape this
	// test: only c.api.X, and the struct literal in New.
	for _, pos := range other {
		t.Errorf("%s uses the Docker client other than to call it", pos)
	}

	// No other package imports the Docker SDK, but for these, each named.
	// memusage reads a container's memory for MemoryUsage and for the
	// image's memory measure: it uses the SDK's types alone and is handed
	// c.api.ContainerStats, counted above, never the client
	// (TestMemusageMakesOnlyTheStatsCall). The measure, a test under the
	// measure tag, makes its own client to read the same number.
	sdkUsers := map[string]bool{
		"internal/docker/memusage":                        true,
		"images/chromium/memflags_measure_docker_test.go": true,
	}
	root := moduleRoot(t)
	own, _ := filepath.Abs(".")
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != root && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules" || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || filepath.Dir(path) == own {
			return nil
		}
		if rel, _ := filepath.Rel(root, path); sdkUsers[filepath.ToSlash(rel)] || sdkUsers[filepath.ToSlash(filepath.Dir(rel))] {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if strings.HasPrefix(p, "github.com/moby/") || strings.HasPrefix(p, "github.com/docker/") {
				rel, _ := filepath.Rel(root, path)
				t.Errorf("%s imports %s; only internal/docker may use the Docker SDK", rel, p)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// TestMemusageMakesOnlyTheStatsCall: memusage, the one package beside this
// one that imports the Docker SDK, uses its stats types alone, so the only
// Docker call it can make is the ContainerStats it is handed.
func TestMemusageMakesOnlyTheStatsCall(t *testing.T) {
	allowedImports := []string{"github.com/moby/moby/client", "github.com/moby/moby/api/types/container"}
	allowedClient := []string{"ContainerStatsOptions", "ContainerStatsResult"}
	files, err := filepath.Glob(filepath.Join("memusage", "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	parsed := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		for _, imp := range f.Imports {
			p, _ := strconv.Unquote(imp.Path.Value)
			if (strings.HasPrefix(p, "github.com/moby/") || strings.HasPrefix(p, "github.com/docker/")) && !slices.Contains(allowedImports, p) {
				t.Errorf("%s imports %s; memusage may use the client's stats types and the container types alone", name, p)
			}
			if imp.Name != nil && (p == "github.com/moby/moby/client") {
				t.Errorf("%s renames the client package; this test looks for client.X", name)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "client" && !slices.Contains(allowedClient, sel.Sel.Name) {
				t.Errorf("%s uses client.%s; memusage may use only %v", fset.Position(sel.Pos()), sel.Sel.Name, allowedClient)
			}
			return true
		})
	}
	if parsed == 0 {
		t.Fatal("found no memusage files: the path is wrong")
	}
}
