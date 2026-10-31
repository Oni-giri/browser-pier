package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/config"
	"github.com/Oni-giri/browser-pier/internal/db"
)

// pinCommand sets the handoff PIN, read from stdin so it stays out of the
// shell's history: echo 482113 | browserpier pin
func pinCommand(in io.Reader, out io.Writer) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil && err != io.EOF { //nolint:errorlint // io.EOF is returned as is
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(cfg.DataDir, "browserpier.db"))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	admin, err := auth.NewAdmin(ctx, d, cfg.AdminPassword, slog.New(slog.DiscardHandler))
	if err != nil {
		return err
	}
	if err := admin.SetPIN(ctx, strings.TrimSpace(line)); err != nil {
		return err
	}
	_, err = fmt.Fprintln(out, "Handoff PIN set.")
	return err
}
