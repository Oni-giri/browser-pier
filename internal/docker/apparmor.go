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

// verdict is a sandbox check's answer for one AppArmor option.
type verdict int

const (
	unknown verdict = iota // the check couldn't tell (the daemon, a timeout…)
	works
	fails // the sandbox fails, or the daemon can't apply the profile
)

// appArmorChoice is where following a plan got.
type appArmorChoice struct {
	opt string
	// settled is false when a check couldn't tell: opt is then the plan's
	// fallback for now, and the next browser start checks again.
	settled bool
	note    AppArmorNote
}

// chooseAppArmor follows a plan. check says whether Chromium's sandbox
// works under an option, and if not why.
//
// Not finding browserpier loaded isn't fatal: browsers then run under
// Docker's default if the sandbox works there, and otherwise unconfined,
// as before BrowserPier had its own profile, with a warning. Refusing
// would break every host that updated without loading it. A check that
// can't tell settles nothing, so a passing hiccup doesn't drop AppArmor
// until the next restart.
func chooseAppArmor(p appArmorPlan, check func(opt string) (verdict, string)) appArmorChoice {
	if p.try == nil {
		return appArmorChoice{opt: p.fixed, settled: true}
	}
	var failed []string
	for _, opt := range p.try {
		v, why := check(opt)
		switch v {
		case works:
			return appArmorChoice{opt: opt, settled: true, note: appArmorNote(opt, failed)}
		case fails:
			failed = append(failed, describeAppArmor(opt)+": "+why)
		default:
			return appArmorChoice{opt: p.fallback, note: AppArmorNote{slog.LevelWarn, fmt.Sprintf(
				"couldn't check whether Chromium's sandbox works %s (%s), so for now browser containers run %s; BrowserPier checks again before the next browser starts",
				describeAppArmor(opt), why, describeAppArmor(p.fallback))}}
		}
	}
	return appArmorChoice{opt: p.fallback, settled: true, note: AppArmorNote{slog.LevelError,
		"Chromium's sandbox fails under every AppArmor choice, so browsers won't start; are user namespaces turned off on the Docker host? Tried " + strings.Join(failed, "; ")}}
}

func appArmorNote(opt string, failed []string) AppArmorNote {
	switch opt {
	case optBrowserPier:
		return AppArmorNote{slog.LevelInfo, "browser containers run under BrowserPier's AppArmor profile, browserpier"}
	case "":
		return AppArmorNote{slog.LevelInfo, "browser containers keep Docker's default AppArmor profile: Chromium's sandbox works under it here"}
	}
	return AppArmorNote{slog.LevelWarn, "browser containers run without an AppArmor profile: Docker's default refuses Chromium's sandbox on this host (AppArmor 4, as on Debian 13), " +
		`and BrowserPier's own profile isn't loaded. Load it to keep AppArmor: with the compose file, "docker compose --profile apparmor up -d"; see "AppArmor" in deploy/README.md. Tried ` + strings.Join(failed, "; ")}
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

// selectAppArmor follows the plan, checking with the browser image, and
// logs the outcome when it says something new. With pull, a missing image
// is pulled first; without, the choice waits for the first browser start.
// The caller holds c.mu.
func (c *Client) selectAppArmor(ctx context.Context, pull bool) {
	var ch appArmorChoice
	switch ready, why := c.checkable(ctx, pull); {
	case c.plan.try == nil || ready:
		ch = chooseAppArmor(c.plan, func(opt string) (verdict, string) { return c.checkSandbox(ctx, opt) })
	case !pull && why == "":
		// Not pulled yet: the first browser start pulls it, and checks.
		ch = appArmorChoice{opt: c.plan.fallback, note: AppArmorNote{slog.LevelInfo,
			"the browser image isn't here yet, so the browsers' AppArmor profile is chosen at the first browser start"}}
	default:
		ch = appArmorChoice{opt: c.plan.fallback, note: AppArmorNote{slog.LevelWarn, fmt.Sprintf(
			"couldn't check which AppArmor profile Chromium's sandbox works under (%s), so for now browser containers run %s; BrowserPier checks again before the next browser starts",
			why, describeAppArmor(c.plan.fallback))}}
	}
	c.apparmorOpt, c.apparmorSettled = ch.opt, ch.settled
	if ch.note.Msg != "" && ch.note != c.apparmorNote {
		c.log.Log(ctx, ch.note.Level, ch.note.Msg)
	}
	c.apparmorNote = ch.note
}

// appArmorForCreate is the AppArmor option for a new browser container,
// checking first if the choice isn't settled yet.
func (c *Client) appArmorForCreate(ctx context.Context) string {
	c.apparmorMu.Lock()
	defer c.apparmorMu.Unlock()
	if !c.apparmorSettled {
		c.selectAppArmor(ctx, true)
	}
	return c.apparmorOpt
}

// LabelCheck marks the short-lived container that checks the sandbox.
const LabelCheck = "io.browserpier.check"

// LabelSandboxCheck on the browser image says it has the sandbox check.
const LabelSandboxCheck = "io.browserpier.sandbox-check"

// checkArg makes the browser image's entrypoint run its sandbox check.
const checkArg = "sandbox-check"

// checkTimeout bounds one check; it takes about a second.
const checkTimeout = 30 * time.Second

// checkable reports whether the browser image is here with its sandbox
// check, pulling it first if pull is set. If not, why says why, or is
// empty when the image just isn't pulled yet.
func (c *Client) checkable(ctx context.Context, pull bool) (bool, string) {
	if c.plan.try == nil {
		return true, ""
	}
	res, err := c.api.ImageInspect(ctx, c.image)
	if cerrdefs.IsNotFound(err) {
		if !pull {
			return false, ""
		}
		if err := c.pull(ctx); err != nil {
			return false, err.Error()
		}
		res, err = c.api.ImageInspect(ctx, c.image)
	}
	if err != nil {
		return false, "inspecting the browser image: " + err.Error()
	}
	if res.Config == nil || res.Config.Labels[LabelSandboxCheck] != "1" {
		return false, fmt.Sprintf("the browser image %s has no sandbox check: it's older than this BrowserPier, so set BROWSERPIER_BROWSER_IMAGE to this release's", c.image)
	}
	return true, ""
}

// checkSandbox runs the browser image's sandbox check in a short-lived
// container, with the browser containers' settings and the AppArmor option
// opt: whether Chromium's sandbox works under it, and if not why. Only the
// daemon refusing the profile, or the check itself failing, counts as
// fails; any other error is unknown.
func (c *Client) checkSandbox(ctx context.Context, opt string) (verdict, string) {
	cfg, host := browserConfig(c.image, c.seccompOpt, opt, BrowserSpec{User: c.user})
	cfg.Cmd = []string{checkArg}
	cfg.Env = nil
	cfg.StopTimeout = nil
	cfg.Labels = map[string]string{LabelCheck: "true", LabelInstance: c.instance}
	host.Mounts = nil
	host.ShmSize = 0
	host.NetworkMode = "none"
	host.UsernsMode = container.UsernsMode(c.userns)
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	// "_" keeps it apart from browsers, named "browserpier-<profile>".
	name := "browserpier_check_" + c.instance
	if err := c.removeCheck(ctx, name); err != nil {
		return unknown, err.Error()
	}
	res, err := c.api.ContainerCreate(ctx, client.ContainerCreateOptions{Name: name, Config: cfg, HostConfig: host})
	if err != nil {
		// A daemon may refuse a profile that isn't loaded when creating.
		return appArmorVerdict(err), shorten(err.Error())
	}
	defer func() {
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = c.api.ContainerRemove(rmCtx, res.ID, client.ContainerRemoveOptions{Force: true})
		cancel()
	}()
	// Not c.Start, which words a missing profile for browsers.
	if _, err := c.api.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		// runc: "unable to apply apparmor profile: ... no such file or
		// directory" when the profile isn't loaded.
		return appArmorVerdict(err), shorten(err.Error())
	}
	code, err := c.Wait(ctx, res.ID)
	if err != nil {
		return unknown, "waiting for the sandbox check: " + err.Error()
	}
	if code == 0 {
		return works, ""
	}
	line, err := c.LastLogLine(ctx, res.ID)
	if err != nil || line == "" {
		line = fmt.Sprintf("exit status %d", code)
	}
	if strings.HasPrefix(line, "sandbox-check: failed") {
		return fails, line
	}
	return unknown, "the sandbox check didn't run: " + line
}

// appArmorVerdict is a create or start error's verdict: fails if the
// daemon couldn't apply the AppArmor profile, unknown otherwise.
func appArmorVerdict(err error) verdict {
	if isAppArmorError(err) {
		return fails
	}
	return unknown
}

func isAppArmorError(err error) bool {
	return err != nil && strings.Contains(strings.ToLower(err.Error()), "apparmor")
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

// ErrAppArmorNotLoaded is a browser container that wouldn't start because
// the AppArmor profile BrowserPier chose isn't loaded on the Docker host
// any more. It checks again before the next browser start, so a second try
// may work.
var ErrAppArmorNotLoaded = errors.New("the browser's AppArmor profile isn't loaded on the Docker host any more")

// ErrAppArmorSetNotLoaded is the same for the profile named in
// BROWSERPIER_BROWSER_APPARMOR, which isn't checked or chosen again: a
// second try would fail the same way.
var ErrAppArmorSetNotLoaded = errors.New("the AppArmor profile set for browsers isn't loaded on the Docker host")

// appArmorStartError words a failure to start a browser container under
// a named AppArmor profile, chosen (auto) or set in
// BROWSERPIER_BROWSER_APPARMOR, that isn't loaded.
func appArmorStartError(err error, profile string, auto bool) error {
	if !isAppArmorError(err) || profile == "" || profile == "unconfined" {
		return err
	}
	if auto {
		return fmt.Errorf("%w: docker couldn't apply %q, loaded when BrowserPier checked (reloading AppArmor on the host unloads it; Umbrel loads it again when the app starts), so it checks again before the next start: %w", ErrAppArmorNotLoaded, profile, err)
	}
	return fmt.Errorf("%w: docker couldn't apply %q, set in BROWSERPIER_BROWSER_APPARMOR; is it loaded? See \"AppArmor\" in deploy/README.md: %w", ErrAppArmorSetNotLoaded, profile, err)
}

// specLabel is the definition version a container gets under an AppArmor
// option: docker.Spec, plus the option when there is one, so that a
// browser started under another AppArmor choice isn't picked up after a
// restart (see sessions.Reconcile).
func specLabel(apparmorOpt string) string {
	if apparmorOpt == "" {
		return Spec
	}
	return Spec + ";" + apparmorOpt
}

// Spec is the definition version this client labels new containers with.
func (c *Client) Spec() string {
	c.apparmorMu.Lock()
	defer c.apparmorMu.Unlock()
	return specLabel(c.apparmorOpt)
}

// Outdated reports whether a container labelled with spec must not run on
// under this BrowserPier: an older definition, or another AppArmor choice.
// While the choice isn't settled, any AppArmor choice is kept.
func (c *Client) Outdated(spec string) bool {
	if base, _, _ := strings.Cut(spec, ";"); base != Spec {
		return true
	}
	c.apparmorMu.Lock()
	defer c.apparmorMu.Unlock()
	return c.apparmorSettled && spec != specLabel(c.apparmorOpt)
}

// AppArmor is the browser containers' AppArmor profile: "unconfined", a
// profile's name, or empty for the daemon's default, or none on a daemon
// without AppArmor. Known after UseDataDir, and may change while the choice
// isn't settled.
func (c *Client) AppArmor() string {
	c.apparmorMu.Lock()
	defer c.apparmorMu.Unlock()
	return strings.TrimPrefix(c.apparmorOpt, "apparmor=")
}
