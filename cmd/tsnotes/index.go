package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"text/tabwriter"

	"github.com/scottjab/tsnotes/internal/config"
	"github.com/scottjab/tsnotes/internal/index"
)

func runIndex(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: tsnotes index <rebuild|sync> [flags]")
	}
	switch args[0] {
	case "rebuild":
		return runIndexRebuild(ctx, args[1:], true)
	case "sync":
		return runIndexRebuild(ctx, args[1:], false)
	default:
		return fmt.Errorf("unknown index subcommand %q; want rebuild or sync", args[0])
	}
}

// runIndexRebuild reconstructs the search index from the markdown files.
//
// rebuild discards the derived rows first and starts clean; sync only reconciles
// what has drifted. Neither touches users, vaults, or shares, which is why this
// is the supported recovery path rather than deleting the database.
func runIndexRebuild(ctx context.Context, args []string, full bool) error {
	name := "sync"
	if full {
		name = "rebuild"
	}
	fs := flag.NewFlagSet("tsnotes index "+name, flag.ContinueOnError)
	var only string
	cfg, err := loadConfig(fs, args, func(fs *flag.FlagSet, _ *config.Config) {
		fs.StringVar(&only, "vault", "", "restrict to one vault directory; default is all of them")
	})
	if err != nil {
		return err
	}

	log := newLogger(cfg.LogLevel)
	a, err := openApp(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	rows, err := a.vaultRows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No vaults yet. One is created the first time someone opens tsnotes.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VAULT\tOWNER\tADDED\tUPDATED\tREMOVED\tUNCHANGED\tERRORS")

	var failed bool
	for _, row := range rows {
		if only != "" && row.Dir != only {
			continue
		}
		v, err := a.vaults.Get(row.Dir)
		if err != nil {
			return fmt.Errorf("open vault %s: %w", row.Dir, err)
		}

		var stats index.Stats
		if full {
			stats, err = a.index.Rebuild(ctx, row.ID, v)
		} else {
			stats, err = a.index.Sync(ctx, row.ID, v)
		}
		if err != nil {
			return fmt.Errorf("%s vault %s: %w", name, row.Dir, err)
		}

		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%d\t%d\t%d\n",
			row.Dir, row.Login, stats.Added, stats.Updated, stats.Removed, stats.Unchanged, len(stats.Errors))
		for _, e := range stats.Errors {
			failed = true
			fmt.Fprintf(os.Stderr, "  %s: %s\n", row.Dir, e)
		}
	}
	w.Flush()

	if failed {
		fmt.Fprintln(os.Stderr, "\nSome notes reported problems. They were still indexed; the messages above say what was wrong.")
	}
	return nil
}
