// Command browserpier is the BrowserPier orchestrator: it serves the API,
// the web UI and the MCP endpoint, and starts and stops browser containers.
package main

import (
	"context"
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
	"strings"
	"syscall"
	"time"

	"github.com/Oni-giri/browser-pier/internal/api"
	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/config"
	"github.com/Oni-giri/browser-pier/internal/db"
	"github.com/Oni-giri/browser-pier/internal/docker"
	"github.com/Oni-giri/browser-pier/internal/sessions"
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
	database, err := db.Open(ctx, filepath.Join(dataDir, "browserpier.db"))
	if err != nil {
		return err
	}
	defer func() { _ = database.Close() }()

	image := cfg.BrowserImage
	if image == "" {
		image = defaultBrowserImage(version)
	}
	dc, err := docker.New(docker.Options{Image: image, UsernsMode: cfg.UsernsMode})
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
	uid, gid := browserUser(os.Getuid(), os.Getgid())
	mgr, err := sessions.NewManager(database, dc, sessions.Layout{DataDir: dataDir, HostDataDir: hostDataDir, UID: uid, GID: gid}, log)
	if err != nil {
		return err
	}
	defer mgr.Close()
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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "listen", ln.Addr().String(), "data_dir", dataDir,
		"host_data_dir", hostDataDir, "browser_image", image)
	agentAPI := api.New(mgr, web, log, cfg.PublicURL)
	go agentAPI.WatchRevocations(ctx, 10*time.Second)
	return serve(ctx, ln, newHandler(web, agentAPI), log)
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
func newHandler(web *auth.Web, agentAPI *api.API) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	web.Register(mux)
	agentAPI.Register(mux)
	// Until the web UI lands (PROJ-11), the home page only confirms the sign-in.
	mux.Handle("GET /{$}", web.RequireAdmin(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><title>BrowserPier</title><p>Signed in to BrowserPier.</p>` +
			`<form method="post" action="/logout"><button>Sign out</button></form>`))
	})))
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
