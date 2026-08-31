package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/scottjab/folio/internal/client"
	"github.com/scottjab/folio/internal/tui"
)

// runTUI starts the terminal client.
//
// Like the MCP bridge, this is a client of a running folio rather than another
// way to open the state directory: it goes through the same JSON API the browser
// uses, so permissions, conflict handling, and link rewriting are the server's
// business here too, and there is no second implementation to keep in step.
func runTUI(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("folio tui", flag.ContinueOnError)
	var (
		server = os.Getenv("FOLIO_SERVER")
		editor string
	)
	fs.StringVar(&server, "server", server, "base URL of a running folio, for example https://folio.your-tailnet.ts.net")
	fs.StringVar(&editor, "editor", "", "editor the e key hands a note to (default $VISUAL, then $EDITOR)")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: folio tui [flags] [note]\n\n"+
			"Opens folio in the terminal. With no note, it opens today's daily note.\n"+
			"A note is a path in your own vault, or vault/path for someone else's.\n\n"+
			"flags:\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 1 {
		return errors.New("at most one note may be named")
	}

	if server == "" {
		server = client.DefaultServer
	}
	cl, err := client.New(server, client.WithUserAgent("folio-tui/"+version))
	if err != nil {
		return err
	}

	// Ask who we are before taking over the screen. An error here is the common
	// case (nothing listening, wrong URL, tailscaled down), and it is far more
	// useful printed to a normal terminal than flashed inside an alternate
	// screen that is about to be torn down.
	if err := preflight(ctx, cl); err != nil {
		return err
	}

	return tui.Run(ctx, tui.Options{
		Client: cl,
		Editor: editor,
		Note:   fs.Arg(0),
	})
}

// preflight checks that the server is there and knows who we are.
func preflight(ctx context.Context, cl *client.Client) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	me, err := cl.Me(ctx)
	switch {
	case err == nil:
		_ = me
		return nil
	case errors.Is(err, client.ErrForbidden):
		return fmt.Errorf("%s does not recognise this machine.\n\n"+
			"folio identifies you by your tailnet address. Is this machine on the same\n"+
			"tailnet as the server?", cl.Server())
	case errors.Is(err, client.ErrUnavailable):
		return fmt.Errorf("%s cannot reach tailscaled right now, so it cannot say who you are.\n\n"+
			"Try again in a moment.", cl.Server())
	default:
		return fmt.Errorf("cannot reach folio at %s: %w\n\n"+
			"Pass --server, or set FOLIO_SERVER. For a local `folio dev`, %s is the default.",
			cl.Server(), err, client.DefaultServer)
	}
}
