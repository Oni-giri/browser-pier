package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/config"
	"github.com/Oni-giri/browser-pier/internal/db"
)

//nolint:gosec // usage text, not a credential
const tokenUsage = `usage:
  browserpier token create NAME [-profiles a,b]   make an agent token and print it once
  browserpier token list                          list tokens (never the tokens themselves)
  browserpier token revoke ID

For setups without the web UI, e.g. docker exec browserpier browserpier token create claude`

// tokenCommand manages agent tokens from the command line, against the
// database in BROWSERPIER_DATA_DIR.
func tokenCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New(tokenUsage)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	d, err := db.Open(ctx, filepath.Join(cfg.DataDir, "browserpier.db"))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	tokens := auth.NewTokens(d)

	switch args[0] {
	case "create":
		fs := flag.NewFlagSet("create", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		scope := fs.String("profiles", "", "comma-separated profiles; all if empty")
		rest := args[1:]
		if len(rest) == 0 || strings.HasPrefix(rest[0], "-") {
			return errors.New(tokenUsage)
		}
		name := rest[0]
		if err := fs.Parse(rest[1:]); err != nil {
			return err
		}
		var list []string
		for _, p := range strings.Split(*scope, ",") {
			if p = strings.TrimSpace(p); p != "" {
				list = append(list, p)
			}
		}
		t, secret, err := tokens.Create(ctx, name, len(list) == 0, list)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(out, "%s\n\nToken %s (%s) for %s. It is shown only this once.\n", secret, t.ID, t.Name, scopeText(t))
		return err
	case "list":
		all, err := tokens.List(ctx)
		if err != nil {
			return err
		}
		for _, t := range all {
			used := "never used"
			if !t.LastUsedAt.IsZero() {
				used = "last used " + t.LastUsedAt.Format("2006-01-02 15:04")
			}
			if _, err := fmt.Fprintf(out, "%s\t%s\t…%s\t%s\t%s\n", t.ID, t.Name, t.Hint, scopeText(t), used); err != nil {
				return err
			}
		}
		return nil
	case "revoke":
		if len(args) != 2 {
			return errors.New(tokenUsage)
		}
		if err := tokens.Revoke(ctx, args[1]); err != nil {
			return err
		}
		_, err := fmt.Fprintf(out, "Revoked %s.\n", args[1])
		return err
	}
	return errors.New(tokenUsage)
}

func scopeText(t auth.Token) string {
	if t.AllProfiles {
		return "all profiles"
	}
	return "profiles " + strings.Join(t.Profiles, ", ")
}
