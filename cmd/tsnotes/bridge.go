package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bridgeStdio connects a stdio-only MCP client to a tsnotes server over HTTP.
//
// Modern MCP clients can speak Streamable HTTP and should point straight at
// <server>/mcp. This exists for the ones that only know how to spawn a process
// and talk JSON-RPC over its pipes.
//
// The bridge is deliberately dumb: it copies JSON-RPC messages between two
// connections and interprets none of them. That means it needs no updating when
// tools change, and it cannot introduce a semantic bug of its own.
//
// Identity still comes from the tailnet. The bridge runs on the user's machine,
// so its HTTP requests arrive at the server from that machine's tailnet address
// and WhoIs identifies them as that user. No token is involved.
func bridgeStdio(ctx context.Context, server string) error {
	endpoint := strings.TrimSuffix(server, "/") + "/mcp"

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	remote := &mcp.StreamableClientTransport{
		Endpoint: endpoint,
		HTTPClient: &http.Client{
			// No overall timeout: the standalone SSE stream is meant to stay
			// open. The per-request deadlines live in the transport.
			Timeout: 0,
		},
	}
	up, err := remote.Connect(ctx)
	if err != nil {
		return fmt.Errorf("connect to %s: %w\n\nIs tsnotes running, and is this machine on the same tailnet?", endpoint, err)
	}
	defer up.Close()

	local := &mcp.StdioTransport{}
	down, err := local.Connect(ctx)
	if err != nil {
		return fmt.Errorf("open stdio transport: %w", err)
	}
	defer down.Close()

	fmt.Fprintf(os.Stderr, "tsnotes: bridging stdio to %s\n", endpoint)

	// Two pumps. Whichever direction fails first tears the other down, so a
	// client that exits does not leave the HTTP connection dangling.
	var wg sync.WaitGroup
	errc := make(chan error, 2)

	pump := func(name string, from, to mcp.Connection) {
		defer wg.Done()
		defer cancel()
		for {
			msg, err := from.Read(ctx)
			if err != nil {
				errc <- fmt.Errorf("%s: %w", name, err)
				return
			}
			if err := to.Write(ctx, msg); err != nil {
				errc <- fmt.Errorf("%s: %w", name, err)
				return
			}
		}
	}

	wg.Add(2)
	go pump("client to server", down, up)
	go pump("server to client", up, down)

	<-ctx.Done()

	// Give the pumps a moment to notice the cancellation before returning, so
	// their errors are the ones reported rather than a bare "context canceled".
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
	}

	close(errc)
	for err := range errc {
		if isCleanShutdown(err) {
			continue
		}
		return err
	}
	return nil
}

// isCleanShutdown reports whether an error just means the other end went away,
// which is how this process is expected to end.
func isCleanShutdown(err error) bool {
	if err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, os.ErrClosed) {
		return true
	}
	s := err.Error()
	return strings.Contains(s, "file already closed") ||
		strings.Contains(s, "use of closed") ||
		strings.Contains(s, "EOF")
}
