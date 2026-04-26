// usher is the entrypoint binary. It dispatches subcommands.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"usher/internal/agent/usheragent"
	"usher/internal/broker"
	"usher/internal/discovery"
	"usher/internal/hook"
	"usher/internal/mainchat"
	"usher/internal/router"
	"usher/internal/sender"
	"usher/internal/web"
)

const Version = "0.1.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	switch cmd {
	case "serve":
		if err := serve(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher serve:", err)
			os.Exit(1)
		}
	case "hook":
		if err := runHook(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher hook:", err)
			os.Exit(1)
		}
	case "setup":
		if err := runSetup(args); err != nil {
			fmt.Fprintln(os.Stderr, "usher setup:", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Println(Version)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintln(os.Stderr, "unknown command:", cmd)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: usher <command> [flags]")
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "commands:")
	fmt.Fprintln(os.Stderr, "  serve              start the web server")
	fmt.Fprintln(os.Stderr, "  setup              install/remove the PreToolUse hook in ~/.claude/settings.json")
	fmt.Fprintln(os.Stderr, "  hook <event-name>  invoked by Claude Code; not for direct use")
	fmt.Fprintln(os.Stderr, "  version            print version")
}

func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ExitOnError)
	addr := fs.String("addr", "127.0.0.1:7777", "listen address")
	projectsDir := fs.String("projects-dir", defaultProjectsDir(), "Claude Code projects directory")
	dataDir := fs.String("data-dir", defaultDataDir(), "usher data directory (XDG_DATA_HOME/usher)")
	claudeCmd := fs.String("claude", "claude", "path to the claude binary")
	permissionMode := fs.String("permission-mode", "default",
		"--permission-mode passed to claude (default|acceptEdits|bypassPermissions|plan)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *projectsDir == "" {
		return fmt.Errorf("could not resolve projects dir; pass --projects-dir")
	}
	if _, err := os.Stat(*projectsDir); err != nil {
		return fmt.Errorf("projects dir %q: %w", *projectsDir, err)
	}

	d, err := discovery.New(*projectsDir, logger)
	if err != nil {
		return err
	}
	b := broker.New()
	sd := sender.New(*claudeCmd, *permissionMode, logger)
	h := hook.New()
	r := router.New(d, sd, b, h)

	mainStore, err := mainchat.NewStore(filepath.Join(*dataDir, "mainchats"))
	if err != nil {
		return fmt.Errorf("init main chat store: %w", err)
	}

	agent := usheragent.NewRule(r)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	if err := d.Start(ctx); err != nil {
		return err
	}

	srv := web.NewServer(*addr, r, mainStore, agent, logger)
	return srv.Run(ctx)
}

func defaultProjectsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".claude", "projects")
}

func defaultDataDir() string {
	if v := os.Getenv("XDG_DATA_HOME"); v != "" {
		return filepath.Join(v, "usher")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".local", "share", "usher")
}
