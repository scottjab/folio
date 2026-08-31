package main

import (
	"context"
	"flag"
	"fmt"
)

// runMCP is the stdio bridge; see bridge.go for the pump itself.
func runMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("tsnotes mcp", flag.ContinueOnError)
	var server string
	fs.StringVar(&server, "server", "", "base URL of a running tsnotes, for example https://tsnotes.your-tailnet.ts.net")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if server == "" {
		return fmt.Errorf("--server is required: give the URL of a running tsnotes on your tailnet\n\n" +
			"Most MCP clients can connect to <server>/mcp directly over HTTP. This bridge is for\n" +
			"clients that only speak stdio.")
	}
	return bridgeStdio(ctx, server)
}
