package docker

// The create guard (PROJ-159). Every container BrowserPier creates goes
// through Client.create, which checks the whole body against a fixed
// envelope, per kind, right before ContainerCreate: the fields BrowserPier
// sets must hold exactly the values written here, and every other field of
// the SDK's structs must be unset.
//
// The envelope is written apart from the builders (browserConfig, Create,
// checkSandbox) on purpose: it restates their constants rather than
// calling them, so a mistake in a builder, or a change that widens a
// container, is refused at run time instead of reaching the daemon. It
// runs in the orchestrator's own process, so it catches BrowserPier's
// mistakes; it is no boundary against an orchestrator that is itself
// compromised, which holds the Docker socket anyway.
//
// To widen it: a new field or value goes here first, by name, then in the
// builder, then in guard_test.go. A new kind of container is a new
// createKind and a new case in checkCreate, written by hand.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"

	"github.com/Oni-giri/browser-pier/images/chromium"
)

// ErrOutsideEnvelope is a container body the guard refused: BrowserPier
// built something it must not create. It is a bug, not a setting.
var ErrOutsideEnvelope = errors.New("outside the containers BrowserPier may create")

// createKind is a kind of container BrowserPier creates: a closed set.
type createKind string

const (
	kindBrowser      createKind = "browser"
	kindSandboxCheck createKind = "sandbox-check"
)

// envelopeError names the kind and the field a body was refused for, never
// a value: the seccomp profile is long, paths show the host's layout, and
// a session's error is read by agents.
type envelopeError struct {
	kind  createKind
	field string
	what  string // what is wrong with it, without its value
}

func (e *envelopeError) Error() string {
	return fmt.Sprintf("refused to create the %s container: %s %s", e.kind, e.field, e.what)
}

func (e *envelopeError) Unwrap() error { return ErrOutsideEnvelope }

// create is the only call to ContainerCreate in BrowserPier. It refuses a
// body outside kind's envelope before anything reaches the daemon.
func (c *Client) create(ctx context.Context, kind createKind, opts client.ContainerCreateOptions) (client.ContainerCreateResult, error) {
	if err := c.checkCreate(kind, opts); err != nil {
		c.log.Error("refused to create a container outside BrowserPier's envelope; this is a bug in BrowserPier, please report it",
			"kind", string(kind), "err", err)
		return client.ContainerCreateResult{}, err
	}
	return c.api.ContainerCreate(ctx, opts)
}

// The envelope's constants, restated here rather than taken from the
// builders.
const (
	guardStopTimeout = 30      // seconds, browsers only
	guardShmSize     = 1 << 30 // /dev/shm of a browser, 1 GiB
	guardCheckCmd    = "sandbox-check"
	guardNoNewPrivs  = "no-new-privileges:true"
)

var (
	// The profile-name pattern of internal/profiles, restated.
	guardProfileRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,30}[a-z0-9])?$`)
	// uid:gid, numeric.
	guardUserRE = regexp.MustCompile(`^([0-9]{1,10}):([0-9]{1,10})$`)
	// The user namespace modes allowed: none, or Podman's keep-id.
	guardUsernsRE = regexp.MustCompile(`^(?:keep-id(?::[a-z]+=[0-9]+(?:,[a-z]+=[0-9]+)*)?)?$`)
)

// The environment variables a browser container may get, and the ones it
// must. A new one for the entrypoint needs its name here.
var (
	guardBrowserEnv = []string{
		"BROWSERPIER_SCREEN",
		"BROWSERPIER_MOUNT_CHECK",
		"BROWSERPIER_GEOIP_URL",
		"BROWSERPIER_GEO_COUNTRY",
		"BROWSERPIER_GEO_TZ",
		"BROWSERPIER_TZ_OVERRIDE",
		"BROWSERPIER_LOCALE_OVERRIDE",
		"BROWSERPIER_LOCAL_NETWORK",
		"BROWSERPIER_HOST_IPS",
		"BROWSERPIER_BLOCK_ADS",
	}
	guardBrowserEnvRequired = []string{"BROWSERPIER_SCREEN", "BROWSERPIER_MOUNT_CHECK"}
)

// guardNamed are the fields checkCreate checks by value, per struct. Every
// other field of these structs, and of the embedded container.Resources,
// must be its zero value.
var guardNamed = map[reflect.Type][]string{
	reflect.TypeFor[client.ContainerCreateOptions](): {"Config", "HostConfig", "Name"},
	reflect.TypeFor[container.Config]():              {"Image", "User", "Labels", "Env", "Cmd", "StopTimeout"},
	reflect.TypeFor[container.HostConfig](): {"CapDrop", "CapAdd", "SecurityOpt", "Mounts", "NetworkMode", "UsernsMode",
		"ShmSize", "Init"},
	reflect.TypeFor[container.Resources](): nil,
	reflect.TypeFor[mount.Mount]():         {"Type", "Source", "Target"},
}

// embeddedSeccomp is the security option that applies the embedded seccomp
// profile inline, as Docker takes it.
var embeddedSeccomp = sync.OnceValue(func() string {
	var b bytes.Buffer
	if err := json.Compact(&b, chromium.SeccompProfile); err != nil {
		return "" // never matches: New fails on the same profile
	}
	return "seccomp=" + b.String()
})

// checkCreate checks a create body against kind's envelope. It reads only
// the body and the Client's fixed settings: the image, user, user
// namespace mode, instance, seccomp option, AppArmor plan and host data
// dir. It mustn't take apparmorMu: Create's choice of AppArmor option may
// run a sandbox check under it, and the plan is set once, in UseDataDir.
func (c *Client) checkCreate(kind createKind, o client.ContainerCreateOptions) error {
	g := guard{kind: kind}
	switch kind {
	case kindBrowser, kindSandboxCheck:
	default:
		return g.refuse("the kind", "is unknown")
	}
	if o.Config == nil {
		return g.refuse("Config", "is unset")
	}
	if o.HostConfig == nil {
		return g.refuse("HostConfig", "is unset")
	}
	// Everything else first: a field nobody named is refused for being set.
	if err := g.unset("ContainerCreateOptions", reflect.ValueOf(o)); err != nil {
		return err
	}
	if err := g.unset("Config", reflect.ValueOf(*o.Config)); err != nil {
		return err
	}
	if err := g.unset("HostConfig", reflect.ValueOf(*o.HostConfig)); err != nil {
		return err
	}
	cfg, host := o.Config, o.HostConfig

	if c.image == "" || cfg.Image != c.image {
		return g.refuse("Config.Image", "isn't the browser image")
	}
	if err := g.user(c.user, cfg.User); err != nil {
		return err
	}
	if !guardUsernsRE.MatchString(c.userns) {
		return g.refuse("HostConfig.UsernsMode", "is neither unset nor keep-id")
	}
	if string(host.UsernsMode) != c.userns {
		return g.refuse("HostConfig.UsernsMode", "isn't the configured mode")
	}
	if !slices.Equal(host.CapDrop, []string{"ALL"}) {
		return g.refuse("HostConfig.CapDrop", "isn't exactly ALL")
	}
	if !slices.Equal(host.CapAdd, []string{"SYS_CHROOT"}) {
		return g.refuse("HostConfig.CapAdd", "isn't exactly SYS_CHROOT")
	}
	apparmor, err := g.securityOpt(c, host.SecurityOpt)
	if err != nil {
		return err
	}
	if host.Init == nil || *host.Init {
		return g.refuse("HostConfig.Init", "isn't set to false")
	}
	if c.instance == "" {
		return g.refuse("Config.Labels", "has no instance to name")
	}

	switch kind {
	case kindBrowser:
		return g.browser(c, o, apparmor)
	default:
		return g.sandboxCheck(c, o)
	}
}

func (g guard) browser(c *Client, o client.ContainerCreateOptions, apparmor string) error {
	cfg, host := o.Config, o.HostConfig
	// Not "" (before UseDataDir) nor relative: the mounts' sources are
	// computed from it, and only an absolute one says where they are.
	if !filepath.IsAbs(c.hostDataDir) {
		return g.refuse("HostConfig.Mounts", "can't be checked before UseDataDir gave an absolute host data dir")
	}
	profile := cfg.Labels[LabelProfile]
	spec := Spec
	if apparmor != "" {
		spec += ";" + apparmor
	}
	if err := g.labels(cfg.Labels, map[string]func(string) bool{
		LabelManaged:  func(v string) bool { return v == "true" },
		LabelInstance: func(v string) bool { return v == c.instance },
		LabelProfile:  guardProfileRE.MatchString,
		LabelSession:  func(v string) bool { return v != "" },
		LabelSpec:     func(v string) bool { return v == spec },
	}); err != nil {
		return err
	}
	if o.Name != "browserpier-"+profile {
		return g.refuse("ContainerCreateOptions.Name", "isn't the profile's container name")
	}
	if err := g.env(cfg.Env); err != nil {
		return err
	}
	if cfg.Cmd != nil {
		return g.refuse("Config.Cmd", "is set")
	}
	if cfg.StopTimeout == nil || *cfg.StopTimeout != guardStopTimeout {
		return g.refuse("Config.StopTimeout", "isn't 30 s")
	}
	if host.NetworkMode != "" {
		return g.refuse("HostConfig.NetworkMode", "isn't the default network")
	}
	if host.ShmSize != guardShmSize {
		return g.refuse("HostConfig.ShmSize", "isn't 1 GiB")
	}
	dir := filepath.Join(c.hostDataDir, "profiles", profile)
	return g.mounts(host.Mounts, map[string]string{
		"/profile":         filepath.Join(dir, "chrome"),
		"/run/browserpier": filepath.Join(dir, "run"),
		"/files":           filepath.Join(dir, "files"),
	})
}

func (g guard) sandboxCheck(c *Client, o client.ContainerCreateOptions) error {
	cfg, host := o.Config, o.HostConfig
	if err := g.labels(cfg.Labels, map[string]func(string) bool{
		LabelCheck:    func(v string) bool { return v == "true" },
		LabelInstance: func(v string) bool { return v == c.instance },
	}); err != nil {
		return err
	}
	if o.Name != "browserpier_check_"+c.instance {
		return g.refuse("ContainerCreateOptions.Name", "isn't the check's container name")
	}
	switch {
	case cfg.Env != nil:
		return g.refuse("Config.Env", "is set")
	case !slices.Equal(cfg.Cmd, []string{guardCheckCmd}):
		return g.refuse("Config.Cmd", "isn't the sandbox check")
	case cfg.StopTimeout != nil:
		return g.refuse("Config.StopTimeout", "is set")
	case host.NetworkMode != "none":
		return g.refuse("HostConfig.NetworkMode", "isn't none")
	case host.ShmSize != 0:
		return g.refuse("HostConfig.ShmSize", "is set")
	case host.Mounts != nil:
		return g.refuse("HostConfig.Mounts", "is set")
	}
	return nil
}

type guard struct{ kind createKind }

func (g guard) refuse(field, what string) error {
	return &envelopeError{kind: g.kind, field: field, what: what}
}

// unset refuses any field of v, a struct, that guardNamed doesn't name and
// that isn't its zero value. An embedded struct is walked as part of v,
// with its fields named after v's (HostConfig.Memory). A nil slice or map
// is unset; an empty one is set: the SDK sends MaskedPaths: [] as an
// empty list, which unmasks every path, where nil keeps the defaults.
func (g guard) unset(prefix string, v reflect.Value) error {
	t := v.Type()
	named, ok := guardNamed[t]
	if !ok {
		// A struct the guard doesn't know: a new embedded type in the SDK.
		if v.IsZero() {
			return nil
		}
		return g.refuse(prefix, "is set")
	}
	for i := range t.NumField() {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			if err := g.unset(prefix, v.Field(i)); err != nil {
				return err
			}
			continue
		}
		if slices.Contains(named, f.Name) {
			continue
		}
		if !v.Field(i).IsZero() {
			return g.refuse(prefix+"."+f.Name, "is set")
		}
	}
	return nil
}

func (g guard) user(want, got string) error {
	m := guardUserRE.FindStringSubmatch(got)
	if m == nil {
		return g.refuse("Config.User", "isn't a numeric uid:gid")
	}
	uid, err := strconv.ParseUint(m[1], 10, 32)
	if err != nil || uid == 0 {
		return g.refuse("Config.User", "is root or out of range")
	}
	if _, err := strconv.ParseUint(m[2], 10, 32); err != nil {
		return g.refuse("Config.User", "is out of range")
	}
	if got != want {
		return g.refuse("Config.User", "isn't the browsers' user")
	}
	return nil
}

// labels checks the exact set of labels, each against its rule.
func (g guard) labels(got map[string]string, want map[string]func(string) bool) error {
	if len(got) != len(want) {
		return g.refuse("Config.Labels", "aren't exactly BrowserPier's")
	}
	for k, ok := range want {
		v, present := got[k]
		if !present {
			return g.refuse("Config.Labels", "lack "+k)
		}
		if !ok(v) {
			return g.refuse("Config.Labels", "have a wrong "+k)
		}
	}
	return nil
}

// env checks a browser's environment by name: each from the list, once,
// the required ones present. Values are checked where they're stored, but
// none may hold a newline or a NUL.
func (g guard) env(env []string) error {
	seen := map[string]bool{}
	for _, e := range env {
		name, value, ok := strings.Cut(e, "=")
		if !ok || !slices.Contains(guardBrowserEnv, name) {
			return g.refuse("Config.Env", "has a variable outside the list")
		}
		if seen[name] {
			return g.refuse("Config.Env", "has a variable twice")
		}
		seen[name] = true
		if strings.ContainsAny(value, "\n\r\x00") {
			return g.refuse("Config.Env", "has a value with a newline or NUL")
		}
	}
	for _, name := range guardBrowserEnvRequired {
		if !seen[name] {
			return g.refuse("Config.Env", "lacks a required variable")
		}
	}
	return nil
}

// securityOpt checks the security options as a set: no-new-privileges,
// the client's seccomp option, and at most one AppArmor option the plan
// allows; nothing else, nothing twice. It returns the AppArmor option.
func (g guard) securityOpt(c *Client, opts []string) (string, error) {
	const field = "HostConfig.SecurityOpt"
	inline := embeddedSeccomp()
	podman := ""
	if filepath.IsAbs(c.hostDataDir) {
		podman = "seccomp=" + filepath.Join(c.hostDataDir, "seccomp.json")
	}
	if c.seccompOpt != inline && (podman == "" || c.seccompOpt != podman) {
		return "", g.refuse(field, "has no seccomp profile BrowserPier ships")
	}
	var nnp, seccomp bool
	apparmor := ""
	for _, o := range opts {
		switch {
		case o == guardNoNewPrivs:
			if nnp {
				return "", g.refuse(field, "has an option twice")
			}
			nnp = true
		case o == c.seccompOpt:
			if seccomp {
				return "", g.refuse(field, "has an option twice")
			}
			seccomp = true
		case strings.HasPrefix(o, "apparmor="):
			if apparmor != "" {
				return "", g.refuse(field, "has two AppArmor options")
			}
			apparmor = o
		default:
			return "", g.refuse(field, "has an option outside the envelope")
		}
	}
	if !nnp {
		return "", g.refuse(field, "lacks no-new-privileges")
	}
	if !seccomp {
		return "", g.refuse(field, "lacks the seccomp profile")
	}
	// The AppArmor plan, read without apparmorMu (see checkCreate).
	p := c.plan
	switch {
	case p.fixed != "":
		if apparmor != p.fixed {
			return "", g.refuse(field, "lacks the AppArmor profile set for browsers")
		}
	case apparmor == "":
	case apparmor == p.fallback || slices.Contains(p.try, apparmor):
	default:
		return "", g.refuse(field, "has an AppArmor option the plan doesn't allow")
	}
	return apparmor, nil
}

// mounts checks a browser's bind mounts: exactly want's targets, each from
// its source, every other field of the mount unset.
func (g guard) mounts(got []mount.Mount, want map[string]string) error {
	const field = "HostConfig.Mounts"
	if len(got) != len(want) {
		return g.refuse(field, "aren't exactly the profile's three folders")
	}
	seen := map[string]bool{}
	for i, m := range got {
		name := fmt.Sprintf("%s[%d]", field, i)
		if err := g.unset(name, reflect.ValueOf(m)); err != nil {
			return err
		}
		if m.Type != mount.TypeBind {
			return g.refuse(name+".Type", "isn't bind")
		}
		src, ok := want[m.Target]
		if !ok || seen[m.Target] {
			return g.refuse(name+".Target", "isn't one of the profile's folders")
		}
		seen[m.Target] = true
		if filepath.Clean(m.Source) != filepath.Clean(src) {
			return g.refuse(name+".Source", "isn't the profile's folder")
		}
	}
	return nil
}
