package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"text/tabwriter"

	"github.com/scottjab/tsnotes/internal/web"
)

// runDoctor prints what tsnotes can see, so a problem can be diagnosed without
// reading the database by hand.
func runDoctor(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tsnotes doctor", flag.ContinueOnError)
	cfg, err := loadConfig(fs, args, nil)
	if err != nil {
		return err
	}

	log := newLogger("error") // the report is the output; logs would clutter it
	a, err := openApp(cfg, log)
	if err != nil {
		return err
	}
	defer a.Close()

	fmt.Printf("tsnotes %s\n\n", version)
	fmt.Printf("state dir   %s\n", cfg.StateDir)
	fmt.Printf("database    %s\n", cfg.DatabasePath())
	fmt.Printf("vaults dir  %s\n", cfg.VaultsDir())
	fmt.Printf("tsnet dir   %s\n", cfg.TSNetDir())
	fmt.Printf("hostname    %s\n", cfg.Hostname)

	schema, err := a.db.SchemaVersion(ctx)
	if err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	fmt.Printf("schema      v%d\n", schema)

	if web.IsPlaceholder() {
		fmt.Printf("web app     NOT BUILT (API and MCP work; the browser UI is a stub)\n")
	} else {
		fmt.Printf("web app     embedded\n")
	}
	if cfg.AuthKey == "" {
		fmt.Printf("auth key    not set (fine after the first run)\n")
	} else {
		fmt.Printf("auth key    set\n")
	}
	fmt.Println()

	rows, err := a.vaultRows(ctx)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		fmt.Println("No vaults yet. One is created the first time someone opens tsnotes.")
		return nil
	}

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "VAULT\tOWNER\tINDEXED\tON DISK\tDRIFT\tTAGS\tLINKS")

	var drifted bool
	for _, row := range rows {
		if !a.dirExists(row.Dir) {
			fmt.Fprintf(w, "%s\t%s\t?\tMISSING\t?\t?\t?\n", row.Dir, row.Login)
			drifted = true
			continue
		}
		v, err := a.vaults.Get(row.Dir)
		if err != nil {
			return fmt.Errorf("open vault %s: %w", row.Dir, err)
		}
		onDisk, err := v.ListNotes()
		if err != nil {
			return fmt.Errorf("list vault %s: %w", row.Dir, err)
		}
		stats, err := a.index.VaultStats(ctx, row.ID)
		if err != nil {
			return fmt.Errorf("stat vault %s: %w", row.Dir, err)
		}

		delta := len(onDisk) - stats.Notes
		if delta != 0 {
			drifted = true
		}
		fmt.Fprintf(w, "%s\t%s\t%d\t%d\t%+d\t%d\t%d\n",
			row.Dir, row.Login, stats.Notes, len(onDisk), delta, stats.Tags, stats.Links)
	}
	w.Flush()

	if drifted {
		fmt.Println("\nThe index and the files disagree. Run `tsnotes index sync` to reconcile,")
		fmt.Println("or `tsnotes index rebuild` to reconstruct the index from the files entirely.")
		fmt.Println("Neither touches users, vaults, or shares.")
	} else {
		fmt.Println("\nThe index matches what is on disk.")
	}

	// Trash grows without bound by design; say how much so it can be cleared.
	if size, count := dirSize(filepath.Join(cfg.VaultsDir())); count > 0 {
		fmt.Printf("\nVault files: %d, %s on disk.\n", count, humanBytes(size))
	}
	return nil
}

func dirSize(root string) (bytes int64, files int) {
	filepath.Walk(root, func(_ string, info os.FileInfo, err error) error {
		if err == nil && info != nil && !info.IsDir() {
			bytes += info.Size()
			files++
		}
		return nil
	})
	return bytes, files
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGT"[exp])
}
