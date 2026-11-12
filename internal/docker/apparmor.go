package docker

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
)

// AppArmorProfile is BrowserPier's own AppArmor profile: Docker's
// docker-default plus the user namespace Chromium's sandbox needs
// (images/chromium/apparmor). The compose file's apparmor service or
// Umbrel's pre-start hook loads it on the host; the orchestrator can't.
const AppArmorProfile = "browserpier"

const (
	optBrowserPier = "apparmor=" + AppArmorProfile
	optUnconfined  = "apparmor=unconfined"
)

// restrictSysctl is Ubuntu's switch for its user namespace restriction.
// Other kernels don't have it. A container sees the host's.
const restrictSysctl = "/proc/sys/kernel/apparmor_restrict_unprivileged_userns"

// RemoteDaemon reports whether the daemon may run on another kernel than
// this process: whenever DOCKER_HOST is set, since even a unix socket can
// be forwarded from elsewhere. Only the default socket counts as local.
func RemoteDaemon() bool { return os.Getenv("DOCKER_HOST") != "" }

// ubuntuUsernsRestriction reports whether the Docker host's kernel has
// Ubuntu's user namespace restriction: "absent", "present" (on, off or
// unreadable), or "unknown" for a remote daemon, whose kernel can't be read.
func ubuntuUsernsRestriction() string {
	if RemoteDaemon() {
		return "unknown"
	}
	if _, err := os.Stat(restrictSysctl); errors.Is(err, fs.ErrNotExist) {
		return "absent"
	}
	return "present"
}

// appArmorPlan is how to pick the browser containers' AppArmor option:
// fixed, or else the first of try that Chromium's sandbox works under,
// with fallback when that can't be checked or none works.
type appArmorPlan struct {
	fixed    string
	try      []string
	fallback string
}

// planAppArmor plans the browser containers' AppArmor option, given the
// daemon's security options, the profile asked for and Ubuntu's restriction
// (see ubuntuUsernsRestriction). Chromium's sandbox needs a user namespace:
//   - Under AppArmor 4 without Ubuntu's restriction (Debian 13, umbrelOS
//     2), Docker's docker-default has no userns rule and refuses it.
//     BrowserPier's own profile allows it, once loaded on the host, and so
//     does running unconfined. Under AppArmor 3 (Debian 12) docker-default
//     works. The version isn't visible from here, so a short-lived
//     container checks, in this order: browserpier, Docker's default, and
//     unconfined as the last resort. The same goes for a remote daemon.
//   - With Ubuntu's restriction, an unconfined process gets a namespace
//     without the capabilities the sandbox needs, and docker-default
//     works. Browsers keep Docker's profile, unchecked.
//
// A profile named in BROWSERPIER_BROWSER_APPARMOR is used as it is, and so
// is a default the admin set for the daemon. A rootless daemon applies no
// AppArmor profile. The seccomp profile, dropped capabilities and
// no-new-privileges apply whatever the choice.
func planAppArmor(daemonOpts []string, profile, ubuntuRestriction string) appArmorPlan {
	if profile != "" {
		return appArmorPlan{fixed: "apparmor=" + profile}
	}
	if ubuntuRestriction == "present" {
		return appArmorPlan{}
	}
	stock := false
	for _, o := range daemonOpts {
		switch o {
		case "name=rootless":
			return appArmorPlan{}
		// Only Docker's stock profile, reported as "name=apparmor" before
		// Docker 29.x and "name=apparmor,profile=default" since. A default
		// the admin set ("name=apparmor,profile=/path") may allow userns.
		case "name=apparmor", "name=apparmor,profile=default":
			stock = true
		}
	}
	if !stock {
		return appArmorPlan{}
	}
	p := appArmorPlan{try: []string{optBrowserPier, "", optUnconfined}}
	if ubuntuRestriction == "absent" {
		// What PROJ-35 chose before there was a check: Debian 13 needs it.
		p.fallback = optUnconfined
	}
	return p
}

// AppArmorNote is what the orchestrator logs about the AppArmor choice;
// empty when there's nothing to say.
type AppArmorNote struct {
	Level slog.Level
	Msg   string
}

// chooseAppArmor follows a plan. check reports whether Chromium's sandbox
// works under an option and if not why, or an error if it couldn't tell.
//
// Not finding browserpier loaded isn't fatal: browsers then run under
// Docker's default if the sandbox works there, and otherwise unconfined,
// as before BrowserPier had its own profile, with a warning. Refusing
// would break every host that updated without loading it.
func chooseAppArmor(p appArmorPlan, check func(opt string) (bool, string, error)) (string, AppArmorNote) {
	if p.try == nil {
		return p.fixed, AppArmorNote{}
	}
	var failed []string
	for _, opt := range p.try {
		ok, why, err := check(opt)
		if err != nil {
			return p.fallback, AppArmorNote{slog.LevelWarn, fmt.Sprintf(
				"couldn't check which AppArmor profile Chromium's sandbox works under (%v), so browser containers run %s", err, describeAppArmor(p.fallback))}
		}
		if ok {
			return opt, appArmorNote(opt, failed)
		}
		failed = append(failed, describeAppArmor(opt)+": "+why)
	}
	return p.fallback, AppArmorNote{slog.LevelError,
		"Chromium's sandbox fails under every AppArmor choice, so browsers won't start; are user namespaces turned off on the Docker host? Tried " + strings.Join(failed, "; ")}
}

func appArmorNote(opt string, failed []string) AppArmorNote {
	switch opt {
	case optBrowserPier:
		return AppArmorNote{slog.LevelInfo, "browser containers run under BrowserPier's AppArmor profile, browserpier"}
	case "":
		return AppArmorNote{slog.LevelInfo, "browser containers keep Docker's default AppArmor profile: Chromium's sandbox works under it here"}
	}
	return AppArmorNote{slog.LevelWarn, "browser containers run without an AppArmor profile: Docker's default refuses Chromium's sandbox on this host (AppArmor 4, as on Debian 13), " +
		`and BrowserPier's own profile isn't loaded. Load it to keep AppArmor: see "AppArmor" in deploy/README.md. Tried ` + strings.Join(failed, "; ")}
}

func describeAppArmor(opt string) string {
	switch opt {
	case "":
		return "with Docker's default AppArmor profile"
	case optUnconfined:
		return "without an AppArmor profile"
	}
	return "under the AppArmor profile " + strings.TrimPrefix(opt, "apparmor=")
}

// LabelCheck marks the short-lived container that checks the sandbox.
const LabelCheck = "io.browserpier.check"

// checkArg makes the browser image's entrypoint run its sandbox check.
const checkArg = "sandbox-check"

// checkSandbox runs the browser image's sandbox check in a short-lived
// container, with the browser containers' settings and the AppArmor option
// opt: whether Chromium's sandbox works under it, and if not why. The
// error is for when it can't tell, say if the image can't be pulled.
func (c *Client) checkSandbox(ctx context.Context, opt string) (bool, string, error) {
	cfg, host := browserConfig(c.image, c.seccompOpt, opt, BrowserSpec{})
	cfg.Cmd = []string{checkArg}
	cfg.Env = nil
	cfg.StopTimeout = nil
	cfg.Labels = map[string]string{LabelCheck: "true", LabelInstance: c.instance}
	host.Mounts = nil
	host.ShmSize = 0
	host.NetworkMode = "none"
	host.UsernsMode = container.UsernsMode(c.userns)
	// "_" keeps it apart from browsers, named "browserpier-<profile>".
	name := "browserpier_check_" + c.instance
	if err := c.removeCheck(ctx, name); err != nil {
		return false, "", err
	}
	opts := client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: host}
	res, err := c.api.ContainerCreate(ctx, opts)
	if cerrdefs.IsNotFound(err) {
		if err := c.pull(ctx); err != nil {
			return false, "", err
		}
		res, err = c.api.ContainerCreate(ctx, opts)
	}
	if err != nil {
		// A daemon may refuse a profile that isn't loaded when creating.
		return false, shorten(err.Error()), nil
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = c.api.ContainerRemove(rmCtx, res.ID, client.ContainerRemoveOptions{Force: true})
		cancel()
	}()
	runCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	// Not c.Start, which words a missing profile for browsers.
	if _, err := c.api.ContainerStart(runCtx, res.ID, client.ContainerStartOptions{}); err != nil {
		// runc: "unable to apply apparmor profile: ... no such file or
		// directory" when the profile isn't loaded.
		return false, shorten(err.Error()), nil
	}
	code, err := c.Wait(runCtx, res.ID)
	if err != nil {
		return false, "", fmt.Errorf("waiting for the sandbox check: %w", err)
	}
	if code == 0 {
		return true, "", nil
	}
	line, err := c.LastLogLine(runCtx, res.ID)
	if err != nil || line == "" {
		line = fmt.Sprintf("exit status %d", code)
	}
	return false, line, nil
}

// removeCheck removes a check container a previous run left behind, say
// if it was killed half way. Only one carrying LabelCheck.
func (c *Client) removeCheck(ctx context.Context, name string) error {
	res, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if res.Container.Config == nil || res.Container.Config.Labels[LabelCheck] != "true" {
		return fmt.Errorf("a container named %s isn't BrowserPier's sandbox check; remove or rename it", name)
	}
	_, err = c.api.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	return err
}

func shorten(s string) string {
	const limit = 300
	if len(s) > limit {
		return strings.ToValidUTF8(s[:limit], "") + "…"
	}
	return s
}

// appArmorStartError words a failure to start a browser container under a
// named AppArmor profile (BROWSERPIER_BROWSER_APPARMOR) that isn't loaded.
func appArmorStartError(err error, profile string) error {
	if err == nil || profile == "" || profile == "unconfined" || !strings.Contains(strings.ToLower(err.Error()), "apparmor") {
		return err
	}
	return fmt.Errorf("docker couldn't apply the AppArmor profile %q to the browser container; is it loaded on the Docker host? See \"AppArmor\" in deploy/README.md: %w", profile, err)
}

// Spec is the definition version this client labels its containers with:
// docker.Spec, plus the AppArmor option when there is one, so that a
// browser started under another AppArmor choice isn't picked up after a
// restart (see sessions.Reconcile).
func (c *Client) Spec() string {
	if c.apparmorOpt == "" {
		return Spec
	}
	return Spec + ";" + c.apparmorOpt
}

// AppArmor is the browser containers' AppArmor profile: "unconfined", a
// profile's name, or empty for the daemon's default, or none on a daemon
// without AppArmor. Known after UseDataDir.
func (c *Client) AppArmor() string { return strings.TrimPrefix(c.apparmorOpt, "apparmor=") }

// AppArmorNote says how the AppArmor profile was chosen, for the log.
// Known after UseDataDir.
func (c *Client) AppArmorNote() AppArmorNote { return c.apparmorNote }
