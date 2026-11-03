// Command browserpier is the BrowserPier orchestrator: it serves the API,
// the web UI and the MCP endpoint, and starts and stops browser containers.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/config"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/files"
	"github.com/Oni-giri/browser-pier/internal/handoff"
	"github.com/Oni-giri/browser-pier/internal/mcp"
	"github.com/Oni-giri/browser-pier/internal/sessions"
	"github.com/Oni-giri/browser-pier/internal/settings"
	"github.com/Oni-giri/browser-pier/internal/ui"
	"github.com/Oni-giri/browser-pier/internal/webrtc"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const shutdownTimeout = 10 * time.Second

func main() {
	showVersion := flag.Bool("version", false, "print the version and exit")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	if flag.Arg(0) == "pin" {
		if err := pinCommand(os.Stdin, os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "browserpier pin:", err)
			os.Exit(1)
		}
		return
	}
	if flag.Arg(0) == "token" {
		if err := tokenCommand(flag.Args()[1:], os.Stdout); err != nil {
			fmt.Fprintln(os.Stderr, "browserpier token:", err)
			os.Exit(1)
		}
		return
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exiting", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dataDir, err := filepath.Abs(cfg.DataDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return err
	}
	privateDir(log, dataDir)
	database, err := db.Open(ctx, filepath.Join(dataDir, "browserpier.db"))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	image := cfg.BrowserImage
	if image == "" {
		image = defaultBrowserImage(version)
	}
	instance, err := instanceID(dataDir)
	if err != nil {
		return err
	}
	dc, err := docker.New(docker.Options{Image: image, UsernsMode: cfg.UsernsMode, Instance: instance, AppArmor: cfg.BrowserAppArmor})
	if err != nil {
		return err
	}
	defer func() { _ = dc.Close() }()
	pingCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	err = dc.Ping(pingCtx)
	cancel()
	if err != nil {
		return fmt.Errorf("can't reach Docker, which BrowserPier needs to start browsers: %w", err)
	}
	hostDataDir, err := dc.HostDataDir(ctx, dataDir, cfg.HostDataDir)
	if err != nil {
		return err
	}
	if err := dc.UseDataDir(ctx, dataDir, hostDataDir); err != nil {
		return err
	}
	switch {
	case cfg.BrowserAppArmor != "":
	case dc.AppArmor() == "unconfined":
		log.Warn("browser containers run without an AppArmor profile: on an AppArmor host without Ubuntu's user namespace restriction, like Debian 13, Docker's docker-default refuses Chromium's sandbox. Set BROWSERPIER_BROWSER_APPARMOR to use one")
	case docker.RemoteDaemon():
		log.Info("Docker is on another machine, so browser containers keep its default AppArmor profile. If browsers fail with a sandbox error (Debian 13 hosts), set BROWSERPIER_BROWSER_APPARMOR=unconfined")
	}
	uid, gid := browserUser(os.Getuid(), os.Getgid())
	mgr, err := sessions.NewManager(database, dc, sessions.Layout{DataDir: dataDir, HostDataDir: hostDataDir, UID: uid, GID: gid}, log)
	if err != nil {
		return err
	}
	defer mgr.Close()
	mgr.SetGeoURL(cfg.GeoIPURL)
	// The addresses live viewers reach the host at are the host's own.
	mgr.SetHostIPs(cfg.WebRTCIPs)
	prefs := settings.New(database)
	saved, err := prefs.Get(ctx)
	if err != nil {
		return fmt.Errorf("reading settings: %w", err)
	}
	// The environment wins over the web UI's setting, as for the admin password.
	ramPercent := cfg.RAMLimitPercent
	if !cfg.RAMLimitFromEnv && saved.RAMLimitPercent > 0 {
		ramPercent = saved.RAMLimitPercent
	}
	total, memErr := hostMemory()
	if ramPercent > 0 {
		if memErr != nil {
			return fmt.Errorf("reading the host's memory for the memory limit: %w", memErr)
		}
		mgr.SetMemoryLimit(total*int64(ramPercent)/100, perBrowser)
	}
	go mgr.WatchIdle(ctx, 30*time.Second)
	if err := mgr.Reconcile(ctx); err != nil {
		return fmt.Errorf("checking browsers left running: %w", err)
	}

	admin, err := auth.NewAdmin(ctx, database, cfg.AdminPassword, log)
	if err != nil {
		return err
	}
	web := auth.NewWeb(admin, auth.NewTokens(database), log, auth.WebOptions{
		TrustedProxies: cfg.TrustedProxies,
		CookieSecure:   cfg.CookieSecure,
		PublicURL:      cfg.PublicURL,
	})

	fileStore := files.New(mgr.Layout().FilesDir, int64(cfg.FilesLimitMB)<<20, uid, gid)
	go fileStore.Sweep(ctx, func() []string {
		// Every profile, not only running ones: a cap lowered in the
		// settings applies to stopped profiles too.
		all, err := mgr.Profiles().List(ctx)
		if err != nil {
			log.Warn("listing profiles for the files cap", "err", err)
			return nil
		}
		names := make([]string, 0, len(all))
		for _, p := range all {
			names = append(names, p.Name)
		}
		return names
	}, time.Minute, func(profile string, err error) {
		log.Warn("enforcing the files cap", "profile", profile, "err", err)
	})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "listen", ln.Addr().String(), "data_dir", dataDir,
		"host_data_dir", hostDataDir, "browser_image", image)
	agentAPI := api.New(mgr, web, fileStore, log, cfg.PublicURL)
	agentAPI.SetSettings(prefs)
	go agentAPI.WatchRevocations(ctx, 10*time.Second)
	relay, err := webrtc.New(webrtc.Options{Port: cfg.WebRTCPort, IPs: cfg.WebRTCIPs}, mgr.Layout().ControlSocket, log)
	if err != nil {
		return err
	}
	defer func() { _ = relay.Close() }()
	live := webrtc.NewHandler(relay, mgr, web, log)
	handoffs := handoff.New(database, mgr, log)
	go handoffs.Sweep(ctx, 15*time.Second)
	agentAPI.SetHandoffs(handoffs)
	app := ui.New(mgr, web, handoffs, prefs, log, ui.Options{
		PublicURL:       cfg.PublicURL,
		HostMemory:      total,
		RAMLimitFromEnv: cfg.RAMLimitFromEnv,
		Version:         version,
		SetRAMLimit: func(percent int) {
			if total > 0 {
				mgr.SetMemoryLimit(total*int64(percent)/100, perBrowser)
				log.Info("memory limit changed", "percent", percent)
			}
		},
	})
	err = serve(ctx, ln, newHandler(web, agentAPI, live, handoff.NewWeb(handoffs, web, live, mgr, log), app, log), log)
	if cfg.StopBrowsersOnExit {
		// After serve: no request can start another browser meanwhile.
		stopBrowsers(mgr, log)
	}
	return err
}

// stopBrowsers stops every browser on the way out; each closes gracefully
// within its container's stop timeout, all at once.
func stopBrowsers(mgr *sessions.Manager, log *slog.Logger) {
	n := len(mgr.List())
	if n == 0 {
		return
	}
	log.Info("stopping every browser before exiting", "browsers", n)
	ctx, cancel := context.WithTimeout(context.Background(), docker.StopTimeout+15*time.Second)
	defer cancel()
	if err := mgr.StopAll(ctx); err != nil {
		log.Error("stopping the browsers", "err", err)
	}
}

// perBrowser is the memory a new browser is expected to need when
// checking it fits under the limit (SPEC §3 plans 1–2 GB each).
const perBrowser = 1 << 30

// hostMemory reads the host's total memory. In a container,
// /proc/meminfo still shows the host's.
func hostMemory() (int64, error) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, err
	}
	return parseMemTotal(string(b))
}

func parseMemTotal(meminfo string) (int64, error) {
	for _, line := range strings.Split(meminfo, "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "MemTotal:" {
			kb, err := strconv.ParseInt(f[1], 10, 64)
			return kb << 10, err
		}
	}
	return 0, errors.New("no MemTotal in /proc/meminfo")
}

// instanceID is this BrowserPier's ID, made on first start and kept in the
// data dir. Its browser containers carry it, so another BrowserPier on the
// same Docker host never adopts or removes them.
func instanceID(dataDir string) (string, error) {
	path := filepath.Join(dataDir, "instance-id")
	if b, err := os.ReadFile(path); err == nil { //nolint:gosec // our own data dir
		if id := strings.TrimSpace(string(b)); id != "" {
			return id, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)
	return id, os.WriteFile(path, []byte(id+"\n"), 0o600)
}

// privateDir makes the data dir its owner's only: it holds the database,
// with proxy credentials, and every profile's logins. A bind mount made
// with a loose umask is tightened; one this user can't chmod is left be.
func privateDir(log *slog.Logger, dir string) {
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm()&0o077 == 0 {
		return
	}
	if err := os.Chmod(dir, 0o700); err != nil { //nolint:gosec // a folder: 0700 is its owner only
		log.Warn("the data dir can be read by other users of this machine", "dir", dir, "err", err)
		return
	}
	log.Info("made the data dir private to its owner", "dir", dir)
}

// defaultBrowserImage is the browser image built alongside this version:
// the release's own tag, or edge for anything else.
func defaultBrowserImage(version string) string {
	const repo = "ghcr.io/oni-giri/browser-pier-chromium"
	if releaseRE.MatchString(version) {
		return repo + ":" + strings.TrimPrefix(version, "v")
	}
	return repo + ":edge"
}

var releaseRE = regexp.MustCompile(`^v\d+\.\d+\.\d+$`)

// browserUser is who browser containers run as: the orchestrator's own
// user, so it owns the profile files, except root, as which Chromium won't
// run with its sandbox on.
func browserUser(uid, gid int) (int, int) {
	if uid == 0 {
		return 1000, 1000
	}
	return uid, gid
}

// newHandler is every route. Cross-origin form posts are refused, so a
// page elsewhere can't act with the admin's cookie.
func newHandler(web *auth.Web, agentAPI *api.API, live *webrtc.Handler, handoffs *handoff.Web, app *ui.UI, log *slog.Logger) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	web.Register(mux)
	agentAPI.Register(mux)
	// MCP tools call the agent API in-process, through routes of their own.
	apiRoutes := http.NewServeMux()
	agentAPI.Register(apiRoutes)
	mcp.New(apiRoutes, web, log, version).Register(mux)
	if live != nil {
		live.Register(mux)
	}
	if handoffs != nil {
		handoffs.Register(mux)
	}
	if app != nil {
		app.Register(mux)
	}
	return new(http.CrossOriginProtection).Handler(mux)
}

// serve runs the HTTP server on ln until ctx is done, then shuts it down gracefully.
func serve(ctx context.Context, ln net.Listener, h http.Handler, log *slog.Logger) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		return err
	}
	if err := <-errc; !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
