package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/nexustar/usher/internal/auth"
)

// runTotp is the worker for `usher totp`: prints the otpauth:// enrollment
// URI (paste into any authenticator app, or open the /totp page for the QR),
// or disables TOTP with --remove. It's the lockout-recovery path when the
// enrolled device is lost.
func runTotp(args []string) error {
	fs := flag.NewFlagSet("totp", flag.ExitOnError)
	dataDir := fs.String("data-dir", defaultDataDir(), "usher data directory (XDG_DATA_HOME/usher)")
	remove := fs.Bool("remove", false, "disable TOTP and delete the secret")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *dataDir == "" {
		return fmt.Errorf("could not resolve data dir; pass --data-dir")
	}
	store, err := auth.Load(*dataDir)
	if err != nil {
		return err
	}
	if *remove {
		if err := store.TotpRemove(); err != nil {
			return err
		}
		fmt.Fprintln(os.Stderr, "two-factor auth disabled; all browser sessions have been kicked.")
		return nil
	}
	_, uri, err := store.TotpEnroll("SR Code", "admin")
	if err != nil {
		return err
	}
	fmt.Println(uri)
	fmt.Fprintln(os.Stderr, "open /totp on the web UI for the QR code.")
	return nil
}
