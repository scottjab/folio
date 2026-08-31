// Command tsnotes is a self-hosted, tailnet-native markdown notes app.
//
// It ships as a single static binary with the web app embedded. Everything it
// keeps lives under one state directory: a SQLite index, the tailnet node's
// state, and one directory of markdown files per user. The markdown is the
// source of truth; the database is a cache that `tsnotes index rebuild` can
// reconstruct from the files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"

	"github.com/scottjab/tsnotes/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "tsnotes: %v\n", err)
		os.Exit(1)
	}
}

// commands is the subcommand table. Keeping it as data rather than a switch
// means usage() cannot drift from what actually runs.
var commands = []struct {
	name  string
	blurb string
	run   func(context.Context, []string) error
}{
	{"serve", "run the server on the tailnet", runServe},
	{"dev", "run locally without a tailnet, for development", runDev},
	{"mcp", "bridge a stdio MCP client to a running tsnotes", runMCP},
	{"index", "rebuild or check the search index", runIndex},
	{"doctor", "report on the state directory and the index", runDoctor},
	{"version", "print the version", runVersion},
}

func run(args []string) error {
	if len(args) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch args[0] {
	case "-h", "--help", "help":
		usage()
		return flag.ErrHelp
	}

	// Ctrl-C and SIGTERM cancel the context, which every command uses to shut
	// down cleanly rather than being killed mid-write.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A second Ctrl-C should kill the process outright. Restoring the default
	// handler once the first signal arrives means someone who does not want to
	// wait for a graceful shutdown does not have to reach for kill.
	go func() {
		<-ctx.Done()
		stop()
	}()

	for _, c := range commands {
		if c.name == args[0] {
			return c.run(ctx, args[1:])
		}
	}
	usage()
	return fmt.Errorf("unknown command %q", args[0])
}

func usage() {
	var b strings.Builder
	b.WriteString("tsnotes: self-hosted markdown notes on your tailnet\n\nusage: tsnotes <command> [flags]\n\ncommands:\n")
	for _, c := range commands {
		fmt.Fprintf(&b, "  %-9s %s\n", c.name, c.blurb)
	}
	b.WriteString("\nRun 'tsnotes <command> -h' for a command's flags.\n")
	fmt.Fprint(os.Stderr, b.String())
}

// commonFlags are the settings every command that touches state needs.
func commonFlags(fs *flag.FlagSet, cfg *config.Config) {
	fs.StringVar(&cfg.StateDir, "state", cfg.StateDir, "directory holding the database, vaults, and tailnet state")
	fs.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "debug, info, warn, or error")
}

// loadConfig assembles the configuration from file, environment, and flags, in
// that order of increasing precedence, then validates the result.
func loadConfig(fs *flag.FlagSet, args []string, extra func(*flag.FlagSet, *config.Config)) (config.Config, error) {
	// The config file has to be found before the other flags are parsed, since
	// it supplies their defaults. A throwaway pass does that without disturbing
	// the real parse.
	path := os.Getenv("TSNOTES_CONFIG")
	pre := flag.NewFlagSet(fs.Name(), flag.ContinueOnError)
	// Unknown flags are expected in this pass and are reported properly by the
	// real parse below, so keep the noise off the terminal.
	pre.SetOutput(io.Discard)
	pre.StringVar(&path, "config", path, "path to a JSON config file")
	pre.Usage = func() {}
	_ = parseIgnoringUnknown(pre, args)

	cfg, err := config.Load(path)
	if err != nil {
		return cfg, err
	}
	cfg.ApplyEnv()

	fs.StringVar(&path, "config", path, "path to a JSON config file")
	commonFlags(fs, &cfg)
	if extra != nil {
		extra(fs, &cfg)
	}
	if err := fs.Parse(args); err != nil {
		return cfg, err
	}
	if err := cfg.Validate(); err != nil {
		return cfg, fmt.Errorf("invalid configuration:\n%w", err)
	}
	return cfg, nil
}

// parseIgnoringUnknown runs a flag set over args, skipping past anything it does
// not recognise. Used only for the config-file pre-pass.
func parseIgnoringUnknown(fs *flag.FlagSet, args []string) error {
	for len(args) > 0 {
		if err := fs.Parse(args); err == nil {
			return nil
		}
		// Drop the offending argument and try again.
		args = args[1:]
		for len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			args = args[1:]
		}
	}
	return nil
}

// newLogger builds the structured logger. Text, not JSON: this runs on someone's
// home server and gets read in a terminal.
func newLogger(level string) *slog.Logger {
	var l slog.Level
	if err := l.UnmarshalText([]byte(level)); err != nil {
		l = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: l}))
}

func runVersion(context.Context, []string) error {
	fmt.Printf("tsnotes %s\n", version)
	if info, ok := debug.ReadBuildInfo(); ok {
		fmt.Printf("go       %s\n", info.GoVersion)
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision", "vcs.time", "GOOS", "GOARCH":
				fmt.Printf("%-8s %s\n", strings.TrimPrefix(s.Key, "vcs."), s.Value)
			}
		}
	}
	return nil
}
