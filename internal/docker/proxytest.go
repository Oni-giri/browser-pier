package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"regexp"
	"slices"
	"time"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/client"
)

// The proxy test: a short-lived container from the browser image runs the
// entrypoint's proxy check (images/chromium/entrypoint/proxycheck.go) on
// the browsers' network, so a proxy is judged from where browsers reach
// it, by the code a start runs. The proxy itself never goes to Docker:
// the caller writes it into the test's own folder, mounted alone at
// /run/browserpier, where the check reads and deletes it.

const (
	// LabelProxyTest marks the proxy test's container. It has no
	// LabelManaged, so List and RemoveStale never take it for a browser.
	LabelProxyTest = "io.browserpier.proxytest"
	// LabelProxyCheck on the browser image says it has the proxy check.
	LabelProxyCheck = "io.browserpier.proxy-check"
	// proxyCheckArg makes the entrypoint run its proxy check.
	proxyCheckArg = "proxy-check"
	// ProxyTestTimeout is all a test gets, from create to remove. The
	// check inside is bounded by a start's 5 s lookup timeout.
	ProxyTestTimeout = 30 * time.Second
	// ProxyTestMemory and ProxyTestPids bound the test's container: a Go
	// program making one HTTPS request. It doesn't count against the
	// browsers' memory limit.
	ProxyTestMemory = 128 << 20
	ProxyTestPids   = 64
	// maxProxyTestOutput is as much of the check's output as is read.
	maxProxyTestOutput = 4 << 10
)

// The proxy test's outcomes: ProxyOK, or why not. A closed set; the UI
// words each one itself.
const (
	ProxyUnreachable = "unreachable"
	ProxyTLS         = "tls_proxy"
	ProxyAuth        = "auth"
	ProxyRefused     = "refused"
	ProxyTimeout     = "timeout"
	ProxyLookup      = "lookup"
	ProxyLookupOff   = "lookup_off"
	ProxyFailed      = "failed"
)

var proxyReasons = []string{ProxyUnreachable, ProxyTLS, ProxyAuth, ProxyRefused, ProxyTimeout, ProxyLookup, ProxyLookupOff, ProxyFailed}

// ErrProxyTestBusy is a second test while one runs: one at a time.
var ErrProxyTestBusy = errors.New("a proxy test is already running")

// ErrNoProxyCheck is a browser image older than the proxy check.
var ErrNoProxyCheck = errors.New("the browser image has no proxy check")

// ProxyTestSpec is what varies between proxy tests.
type ProxyTestSpec struct {
	// Dir is the test's own folder, as the Docker daemon sees it, holding
	// the proxy file. Nothing else is mounted.
	Dir string
	// GeoURL is the browsers' location lookup endpoint (never empty: with
	// the lookup off, there is nothing to test).
	GeoURL string
}

// ProxyResult is a proxy test's outcome, every field validated.
type ProxyResult struct {
	OK bool
	// Where the proxy exits, and what a browser there would run with.
	IP       netip.Addr
	Country  string
	TimeZone string
	Locale   string
	Accept   []string
	// Lookup is how long the one lookup took, proxy handshake included.
	Lookup time.Duration
	// Reason is why not, when not OK: one of the Proxy* outcomes.
	Reason string
}

// ProxyTestName is the test container's name. "_" keeps it apart from
// browsers ("browserpier-<profile>"), and "proxytest" from the sandbox
// check ("browserpier_check_<instance>").
func ProxyTestName(instance string) string { return "browserpier_proxytest_" + instance }

// proxyTestConfig is the test container's definition: a browser's,
// network included, less everything a browser needs and the check
// doesn't (profile, files, shm, SYS_CHROOT, stop timeout), with the test's
// folder as its only mount, and small hard limits.
func proxyTestConfig(image, seccompOpt, apparmorOpt, user, userns, instance string, spec ProxyTestSpec) (*container.Config, *container.HostConfig) {
	cfg, host := browserConfig(image, seccompOpt, apparmorOpt, BrowserSpec{User: user})
	cfg.Cmd = []string{proxyCheckArg}
	cfg.Env = []string{"BROWSERPIER_GEOIP_URL=" + spec.GeoURL}
	cfg.StopTimeout = nil
	cfg.Labels = map[string]string{LabelProxyTest: "true", LabelInstance: instance}
	host.Mounts = []mount.Mount{{Type: mount.TypeBind, Source: spec.Dir, Target: "/run/browserpier"}}
	host.CapAdd = nil
	host.ShmSize = 0
	host.UsernsMode = container.UsernsMode(userns)
	pids := int64(ProxyTestPids)
	host.Memory = ProxyTestMemory
	host.MemorySwap = ProxyTestMemory // no swap on top
	host.PidsLimit = &pids
	return cfg, host
}

// proxyTestOptions is the proxy test's create body under the AppArmor
// option opt, as the create guard checks it (kindProxyTest).
func (c *Client) proxyTestOptions(opt string, spec ProxyTestSpec) client.ContainerCreateOptions {
	cfg, host := proxyTestConfig(c.image, c.seccompOpt, opt, c.user, c.userns, c.instance, spec)
	return client.ContainerCreateOptions{Name: ProxyTestName(c.instance), Config: cfg, HostConfig: host}
}

// TestProxy runs the proxy check in a short-lived container and returns
// what it found. An error means the test didn't run (ErrProxyTestBusy,
// ErrNoProxyCheck, ErrOutsideEnvelope, or the daemon's own); its text holds nothing of the
// proxy, which this package never sees. The container is gone when it
// returns, whatever happened.
func (c *Client) TestProxy(ctx context.Context, spec ProxyTestSpec) (ProxyResult, error) {
	if !c.proxyTestMu.TryLock() {
		return ProxyResult{}, ErrProxyTestBusy
	}
	defer c.proxyTestMu.Unlock()
	if err := c.proxyCheckable(ctx); err != nil {
		return ProxyResult{}, err
	}
	opt := c.appArmorForCreate(ctx)
	ctx, cancel := context.WithTimeout(ctx, ProxyTestTimeout)
	defer cancel()
	name := ProxyTestName(c.instance)
	if err := c.removeProxyTest(ctx, name); err != nil {
		return ProxyResult{}, err
	}
	res, err := c.create(ctx, kindProxyTest, c.proxyTestOptions(opt, spec))
	if errors.Is(err, ErrOutsideEnvelope) {
		return ProxyResult{}, err // the guard refused it: nothing reached the daemon
	}
	if err != nil {
		// Cut short while the daemon was creating it, the container may
		// exist all the same: remove it by name (label checked).
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_ = c.removeProxyTest(rmCtx, name)
		cancel()
		return ProxyResult{}, fmt.Errorf("creating the proxy test's container: %w", err)
	}
	defer func() {
		// Even once ctx is done: a test cut short leaves nothing behind.
		rmCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		_, _ = c.api.ContainerRemove(rmCtx, res.ID, client.ContainerRemoveOptions{Force: true})
		cancel()
	}()
	if _, err := c.api.ContainerStart(ctx, res.ID, client.ContainerStartOptions{}); err != nil {
		return ProxyResult{}, fmt.Errorf("starting the proxy test's container: %w", err)
	}
	code, err := c.Wait(ctx, res.ID)
	if err != nil {
		return ProxyResult{}, fmt.Errorf("waiting for the proxy test: %w", err)
	}
	out, err := c.checkOutput(ctx, res.ID)
	if err != nil {
		return ProxyResult{}, fmt.Errorf("reading the proxy test's result: %w", err)
	}
	r, err := ParseProxyResult(out, code)
	if err != nil {
		return ProxyResult{}, err
	}
	return r, nil
}

// proxyCheckable checks the browser image is here, pulling it if not, and
// has the proxy check.
func (c *Client) proxyCheckable(ctx context.Context) error {
	res, err := c.api.ImageInspect(ctx, c.image)
	if cerrdefs.IsNotFound(err) {
		if err := c.pull(ctx); err != nil {
			return err
		}
		res, err = c.api.ImageInspect(ctx, c.image)
	}
	if err != nil {
		return fmt.Errorf("inspecting the browser image: %w", err)
	}
	if res.Config == nil || res.Config.Labels[LabelProxyCheck] != "1" {
		return ErrNoProxyCheck
	}
	return nil
}

// removeProxyTest removes a test container a previous run left behind,
// say if it was killed half way: only one with this instance's
// LabelProxyTest. Another container by that name is left alone.
func (c *Client) removeProxyTest(ctx context.Context, name string) error {
	res, err := c.api.ContainerInspect(ctx, name, client.ContainerInspectOptions{})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if res.Container.Config == nil || res.Container.Config.Labels[LabelProxyTest] != "true" || res.Container.Config.Labels[LabelInstance] != c.instance {
		return fmt.Errorf("a container named %s isn't BrowserPier's proxy test; remove or rename it", name)
	}
	_, err = c.api.ContainerRemove(ctx, name, client.ContainerRemoveOptions{Force: true})
	if cerrdefs.IsNotFound(err) {
		return nil
	}
	return err
}

// RemoveProxyTest removes a test container left behind by a run that was
// killed, as at start-up. A test running in this process is left alone.
func (c *Client) RemoveProxyTest(ctx context.Context) error {
	if !c.proxyTestMu.TryLock() {
		return nil
	}
	defer c.proxyTestMu.Unlock()
	return c.removeProxyTest(ctx, ProxyTestName(c.instance))
}

// checkOutput reads the check's standard output, at most
// maxProxyTestOutput of it (more is refused by the parser): not
// LastLogLine, which is tuned for browsers' logs.
func (c *Client) checkOutput(ctx context.Context, id string) ([]byte, error) {
	r, err := c.api.ContainerLogs(ctx, id, client.ContainerLogsOptions{ShowStdout: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	var out limitedBuffer
	out.limit = maxProxyTestOutput + 1
	_, err = stdcopy.StdCopy(&out, io.Discard, r)
	if errors.Is(err, errOutputFull) {
		err = nil
	}
	return out.Bytes(), err
}

var errOutputFull = errors.New("output over the limit")

// limitedBuffer keeps the first limit bytes written, then refuses more.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); len(p) > room {
		b.Buffer.Write(p[:room])
		return room, errOutputFull
	}
	return b.Buffer.Write(p)
}

var (
	resultCountry = regexp.MustCompile(`^[A-Z]{2}$`)
	// A BCP 47 tag as the check makes them: a language, and maybe a region.
	resultLang = regexp.MustCompile(`^[a-z]{2,3}(?:-[A-Z]{2})?$`)
)

// ParseProxyResult reads the check's output, which came from a container
// that talked to a third party through a third party, so nothing in it is
// trusted: one JSON line, no unknown fields, every value checked, and the
// exit code agreeing. Anything else is an error, which the caller reports
// as ProxyFailed.
func ParseProxyResult(out []byte, exitCode int) (ProxyResult, error) {
	bad := func(what string) (ProxyResult, error) {
		return ProxyResult{}, errors.New("the proxy test's result is unusable: " + what)
	}
	if len(out) > maxProxyTestOutput {
		return bad("too long")
	}
	var lines [][]byte
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		if l := bytes.TrimSpace(sc.Bytes()); len(l) > 0 {
			lines = append(lines, l)
		}
	}
	if sc.Err() != nil || len(lines) != 1 {
		return bad("not one line")
	}
	var v struct {
		OK       *bool    `json:"ok"`
		IP       string   `json:"ip"`
		Country  string   `json:"country"`
		TimeZone string   `json:"time_zone"`
		Locale   string   `json:"locale"`
		Accept   []string `json:"accept_languages"`
		MS       int64    `json:"ms"`
		Reason   string   `json:"reason"`
	}
	dec := json.NewDecoder(bytes.NewReader(lines[0]))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&v); err != nil || v.OK == nil {
		return bad("not the check's JSON")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return bad("more after the check's JSON")
	}
	if !*v.OK {
		if exitCode == 0 || !slices.Contains(proxyReasons, v.Reason) ||
			v.IP != "" || v.Country != "" || v.TimeZone != "" || v.Locale != "" || v.Accept != nil || v.MS != 0 {
			return bad("not a failure the check reports")
		}
		return ProxyResult{Reason: v.Reason}, nil
	}
	if exitCode != 0 || v.Reason != "" {
		return bad("a success that failed")
	}
	ip, err := netip.ParseAddr(v.IP)
	if err != nil || ip.Zone() != "" {
		return bad("the IP")
	}
	if !resultCountry.MatchString(v.Country) {
		return bad("the country")
	}
	if v.TimeZone == "" || v.TimeZone == "Local" {
		return bad("the time zone")
	}
	if _, err := time.LoadLocation(v.TimeZone); err != nil {
		return bad("the time zone")
	}
	if !resultLang.MatchString(v.Locale) || len(v.Accept) == 0 || len(v.Accept) > 8 {
		return bad("the languages")
	}
	for _, l := range v.Accept {
		if !resultLang.MatchString(l) {
			return bad("the languages")
		}
	}
	if v.MS <= 0 || v.MS > ProxyTestTimeout.Milliseconds() {
		return bad("the time")
	}
	return ProxyResult{OK: true, IP: ip, Country: v.Country, TimeZone: v.TimeZone, Locale: v.Locale, Accept: v.Accept,
		Lookup: time.Duration(v.MS) * time.Millisecond}, nil
}
