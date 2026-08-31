package httpapi

import (
	"context"
	"encoding/json/jsontext"
	"net/http"
	"time"

	"github.com/scottjab/tsnotes/internal/events"
	"github.com/scottjab/tsnotes/internal/share"
)

// sseKeepalive is how often we send a comment frame on an idle stream, to keep
// proxies and NAT tables from deciding the connection is dead.
const sseKeepalive = 25 * time.Second

// sseBuffer is how many events one browser may fall behind by before we start
// dropping. A browser that far behind should reload rather than replay.
const sseBuffer = 64

// handleEvents streams note changes to the browser over server-sent events.
//
// This is what makes an edit in Obsidian, or from an agent over MCP, appear in
// an open editor tab without a refresh. Events are filtered per subscriber
// against the same share rules as everything else, so a stream never carries the
// existence of a note the viewer cannot read.
func (a *API) handleEvents(w http.ResponseWriter, r *http.Request) {
	u := userFrom(r.Context())

	flusher, ok := w.(http.Flusher)
	if !ok {
		a.fail(w, r, http.StatusInternalServerError, errNoFlush)
		return
	}

	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache, no-transform")
	h.Set("Connection", "keep-alive")
	// Tell any intermediary not to buffer, or events arrive in bursts minutes
	// late, which looks exactly like the feature being broken.
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	queue := make(chan events.NoteChanged, sseBuffer)
	cancel := a.Bus.On(func(_ context.Context, e events.NoteChanged) {
		// The permission check happens per event, not once at subscribe time,
		// so revoking a share takes effect on an already-open stream.
		if err := a.Shares.Check(r.Context(), u, e.VaultID, e.Path, share.Read); err != nil {
			return
		}
		select {
		case queue <- e:
		default:
			// This browser is not keeping up. Dropping is right: it will resync
			// on its next fetch.
		}
	})
	defer cancel()

	enc := jsontext.NewEncoder(w)
	ticker := time.NewTicker(sseKeepalive)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-a.closed.Done():
			// The server is shutting down. Ending the response lets Shutdown
			// return immediately; the browser reconnects on its own if we come
			// back.
			return
		case e := <-queue:
			if err := writeSSE(w, enc, e.ID, e.Kind, e); err != nil {
				return
			}
			flusher.Flush()
		case <-ticker.C:
			if _, err := w.Write([]byte(": keepalive\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
