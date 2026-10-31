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
	"strings"
	"syscall"
	"time"

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
	instance, err := instanceID(dataDir)
	if err != nil {
		return err
	}
	dc, err := docker.New(docker.Options{Image: image, UsernsMode: cfg.UsernsMode, Instance: instance})
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

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	log.Info("starting", "version", version, "listen", ln.Addr().String(), "data_dir", dataDir,
		"host_data_dir", hostDataDir, "browser_image", image)
	return serve(ctx, ln, newHandler(), log)
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

func newHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte("ok\n"))
	})
	return mux
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
