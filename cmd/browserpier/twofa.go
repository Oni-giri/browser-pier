package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"path/filepath"

	"github.com/Oni-giri/browser-pier/internal/auth"
	"github.com/Oni-giri/browser-pier/internal/config"
	"github.com/Oni-giri/browser-pier/internal/db"
)

const twoFAUsage = "usage: browserpier 2fa status | browserpier 2fa reset"

// twoFACommand shows or resets the admin's two-step sign-in. It needs no
// password: whoever can run it inside the container already holds the
// Docker socket. It works while the server runs, which reads the state
// at every sign-in, and leaves sessions and the password as they are.
func twoFACommand(args []string, out io.Writer) error {
	if len(args) != 1 || (args[0] != "status" && args[0] != "reset") {
		return errors.New(twoFAUsage)
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
	if args[0] == "reset" {
		wasOn, codes, err := auth.ResetTwoStep(ctx, d)
		if err != nil {
			return err
		}
		if !wasOn && codes == 0 {
			_, err = fmt.Fprintln(out, "Two-step sign-in was already off. Nothing changed.")
			return err
		}
		_, err = fmt.Fprintf(out, "Two-step sign-in is off: removed the authenticator secret and %d recovery %s. The password alone signs in now; signed-in browsers stay signed in.\n",
			codes, plural(codes, "code", "codes"))
		return err
	}
	st, err := auth.ReadTwoStep(ctx, d)
	if err != nil {
		if errors.Is(err, auth.ErrTwoStepUnreadable) {
			_, err = fmt.Fprintln(out, "Two-step sign-in: its state is partial or can't be read, so sign-ins are refused. Run `browserpier 2fa reset`.")
		}
		return err
	}
	if !st.On {
		_, err = fmt.Fprintln(out, "Two-step sign-in: off.")
		return err
	}
	since := ""
	if !st.Since.IsZero() {
		since = " since " + st.Since.Format("2006-01-02")
	}
	_, err = fmt.Fprintf(out, "Two-step sign-in: on%s, %d recovery %s left.\n", since, st.CodesLeft, plural(st.CodesLeft, "code", "codes"))
	return err
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}
